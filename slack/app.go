package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"

	"github.com/nanzhong/tester"
	"github.com/nanzhong/tester/alerting"
	"github.com/nanzhong/tester/scheduler"
	"github.com/nlopes/slack"
	"golang.org/x/sync/errgroup"
)

type options struct {
	username       string
	webhookURL     string
	signingSecret  string
	customChannels map[string][]string

	baseURL   string
	scheduler *scheduler.Scheduler
}

type Option func(*options)

func WithBaseURL(url string) Option {
	return func(opts *options) {
		opts.baseURL = url
	}
}

func WithUsername(username string) Option {
	return func(opts *options) {
		opts.username = username
	}
}

func WithWebhookURL(webhookURL string) Option {
	return func(opts *options) {
		opts.webhookURL = webhookURL
	}
}

func WithSigningSecret(signingSecret string) Option {
	return func(opts *options) {
		opts.signingSecret = signingSecret
	}
}

func WithCustomChannels(channels map[string][]string) Option {
	return func(opts *options) {
		opts.customChannels = channels
	}
}

func WithScheduler(scheduler *scheduler.Scheduler) Option {
	return func(opts *options) {
		opts.scheduler = scheduler
	}
}

type App struct {
	packages []tester.Package

	*options

	usageMessage *slack.Message
}

func NewApp(packages []tester.Package, opts ...Option) *App {
	defOpts := &options{
		username: "tester",
	}

	for _, opt := range opts {
		opt(defOpts)
	}

	return &App{
		options: defOpts,

		packages: packages,
	}
}

func (s *App) HandleSlackCommand(w http.ResponseWriter, r *http.Request) {
	verifier, err := slack.NewSecretsVerifier(r.Header, s.signingSecret)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	r.Body = ioutil.NopCloser(io.TeeReader(r.Body, &verifier))
	cmd, err := slack.SlashCommandParse(r)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = verifier.Ensure(); err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	if s.scheduler == nil {
		message := &slack.Msg{
			Text: ":warning: Slack integration not configured for scheduling tests.",
		}

		json.NewEncoder(w).Encode(message)
		return
	}

	args := strings.Fields(cmd.Text)
	if len(args) < 1 {
		message := &slack.Msg{
			Text: fmt.Sprintf(":warning: Missing action. See `%s help`.", cmd.Command),
		}

		json.NewEncoder(w).Encode(message)
		return
	}

	switch strings.ToLower(args[0]) {
	case "help":
		json.NewEncoder(w).Encode(s.helpMessage(cmd.Command))
		return
	case "test":
		// continue through to handling the action.
	default:
		message := &slack.Msg{
			Text: fmt.Sprintf(":warning: Unknown action *%s*. See `%s help`.", args[0], cmd.Command),
		}

		json.NewEncoder(w).Encode(message)
		return
	}

	packageName := args[1]
	args = args[2:]

	pkg, err := s.getPackage(packageName)
	if err != nil {
		message := &slack.Msg{
			Text: fmt.Sprintf(":warning: Failed to schedule test run for package %s: *%s*", packageName, err),
		}

		json.NewEncoder(w).Encode(message)
		return
	}

	run, err := s.scheduler.Schedule(r.Context(), packageName, args...)
	if err != nil {
		message := &slack.Msg{
			Text: fmt.Sprintf(":warning: Failed to schedule test run for package %s: *%s*", packageName, err),
		}

		json.NewEncoder(w).Encode(message)
		return
	}
	runURL := fmt.Sprintf("%s/runs/%s", s.baseURL, run.ID)

	messageText := slack.NewTextBlockObject(slack.MarkdownType, fmt.Sprintf(":traffic_light:  *NEW* - Started new test run for package %s\n%s", packageName, runURL), false, false)
	messageSection := slack.NewSectionBlock(messageText, nil, nil)

	runDetail := slack.Attachment{
		Color:     "#80cee1",
		Title:     run.Package,
		TitleLink: runURL,
		Fields: []slack.AttachmentField{
			{
				Title: "Run ID",
				Value: run.ID.String(),
			},
		},

		Footer:     "tester",
		FooterIcon: "",
		Ts:         json.Number(strconv.FormatInt(run.EnqueuedAt.Unix(), 10)),
	}

	var options []string
	for _, option := range pkg.Options {
		options = append(options, fmt.Sprintf("`%s`", option.String()))
	}
	if len(options) > 0 {
		runDetail.Fields = append(runDetail.Fields, slack.AttachmentField{
			Title: "Options",
			Value: strings.Join(options, "\n"),
		})
	}

	message := slack.NewBlockMessage(
		messageSection,
	)
	message.ResponseType = slack.ResponseTypeInChannel
	message.Attachments = append(message.Attachments, runDetail)

	json.NewEncoder(w).Encode(message)
}

func (a *App) Fire(ctx context.Context, alert *alerting.Alert) error {
	testLink := fmt.Sprintf("%s/tests/%s", alert.BaseURL, alert.Test.ID)

	messageText := slack.NewTextBlockObject(slack.MarkdownType, fmt.Sprintf(":warning: *FAIL* - %s\n%s", alert.Test.Result.Name, testLink), false, false)
	messageSection := slack.NewSectionBlock(messageText, nil, nil)

	testDetail := slack.Attachment{
		Color:     "#ff005f",
		Title:     alert.Test.Result.Name,
		TitleLink: testLink,
		Fields: []slack.AttachmentField{
			{
				Title: "Test ID",
				Value: alert.Test.ID.String(),
				Short: true,
			},
			{
				Title: "Duration",
				Value: alert.Test.Result.Duration().String(),
				Short: true,
			},
		},

		Footer:     "tester",
		FooterIcon: "",
		Ts:         json.Number(strconv.FormatInt(alert.Test.Result.FinishedAt.Unix(), 10)),
	}

	pkg, err := a.getPackage(alert.Test.Package)
	if err != nil {
		return fmt.Errorf("firing slack alert: %w", err)
	}

	var options []string
	for _, option := range pkg.Options {
		options = append(options, fmt.Sprintf("`%s`", option.String()))
	}
	if len(options) > 0 {
		testDetail.Fields = append(testDetail.Fields, slack.AttachmentField{
			Title: "Options",
			Value: strings.Join(options, "\n"),
		})
	}

	channels, ok := a.customChannels[pkg.Name]
	if !ok {
		channels = []string{""}
	}

	var eg errgroup.Group
	for _, channel := range channels {
		eg.Go(func() error {
			return slack.PostWebhook(a.webhookURL, &slack.WebhookMessage{
				Username: a.username,
				Channel:  channel,
				Blocks: []slack.Block{
					messageSection,
				},
				Attachments: []slack.Attachment{
					testDetail,
				},
			})
		})
	}
	err = eg.Wait()
	if err != nil {
		return fmt.Errorf("firing slack alert: %w", err)
	}
	return nil
}

func (a *App) helpMessage(command string) *slack.Message {
	if a.usageMessage != nil {
		return a.usageMessage
	}

	// for readability here
	lines := []string{
		"```",
		"Trigger tests from Slack",
		"",
		fmt.Sprintf("Usage: %s <action> [arguments]", command),
		"",
		"The commands are:",
		"",
		"  help                      print this help message",
		"  test <package> [options]  trigger an e2e test",
		"",
		"Test packages:",
	}
	for _, pkg := range a.scheduler.Packages {
		lines = append(lines, "", fmt.Sprintf("  %s", pkg.Name))
		for _, option := range pkg.Options {
			description := fmt.Sprintf("      %s", option.Description)
			if option.Default != "" {
				description = description + fmt.Sprintf(" (default: %s)", option.Default)
			}
			lines = append(lines, fmt.Sprintf("    -%s", option.Name), description)
		}
	}
	lines = append(lines, "```")

	message := slack.NewBlockMessage(slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, strings.Join(lines, "\n"), false, false), nil, nil))
	a.usageMessage = &message
	return a.usageMessage
}

func (a *App) getPackage(name string) (*tester.Package, error) {
	for _, p := range a.packages {
		if p.Name == name {
			return &p, nil
		}
	}
	return nil, fmt.Errorf("unknown package: %s", name)
}
