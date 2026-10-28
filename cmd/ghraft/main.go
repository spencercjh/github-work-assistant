package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github-raft-app/internal/app"
	"github-raft-app/internal/bootstrap"
)

var version = "dev"

func readSecret(env, fallback string) ([]byte, error) {
	p := os.Getenv(env)
	if p == "" {
		p = fallback
	}
	b, err := app.ReadPrivateFile(p)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", env, err)
	}
	return b, nil
}
func run() error {
	if len(os.Args) > 1 && os.Args[1] == "init" {
		return bootstrap.Init(os.Args[2:], os.Stdout)
	}
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Fprintln(os.Stdout, version)
		return nil
	}
	flags := flag.NewFlagSet("ghraft", flag.ContinueOnError)
	conf := flags.String("config", "config.json", "Configuration path")
	if e := flags.Parse(os.Args[1:]); e != nil {
		return e
	}
	command := "serve"
	if flags.NArg() > 0 {
		command = flags.Arg(0)
	}
	c, e := app.LoadConfig(*conf)
	if e != nil {
		return e
	}
	if command == "check-config" {
		fmt.Fprintln(os.Stdout, "configuration is valid")
		return nil
	}
	if command == "manifest" {
		if c.RaftMode != "app" {
			return errors.New("manifest is only available in app mode")
		}
		s := app.Service{Config: c}
		return json.NewEncoder(os.Stdout).Encode(s.Manifest())
	}
	dsn, e := readSecret("GHRAFT_DSN_FILE", c.PrivateFile("dsn"))
	if e != nil {
		return e
	}
	raw, e := readSecret("GHRAFT_KEY_FILE", c.PrivateFile("key"))
	if e != nil {
		return e
	}
	key, e := hex.DecodeString(string(raw))
	if e != nil || len(key) != 32 {
		return errors.New("GHRAFT_KEY_FILE must contain a 32-byte hex AES key")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	dbctx, dbCancel := context.WithTimeout(ctx, 20*time.Second)
	defer dbCancel()
	store, e := app.OpenStoreBackend(dbctx, c.DatabaseDriver, c.DatabaseLocation(string(dsn)), key, command == "migrate")
	if e != nil {
		return e
	}
	defer store.Close()
	store.LookbackHours = c.LookbackHours
	store.PrivateSince, _ = time.Parse(time.RFC3339, c.PrivateNotificationsSince)
	if command == "migrate" {
		return store.Migrate(dbctx)
	}
	secret := ""
	if c.RaftMode == "app" && os.Getenv("GHRAFT_CLIENT_SECRET_FILE") != "" {
		b, e := readSecret("GHRAFT_CLIENT_SECRET_FILE", "")
		if e != nil {
			return e
		}
		secret = string(b)
	}
	raft := &app.Raft{Config: c, Secret: secret, HTTP: app.SafeHTTP(), Store: store}
	needsClaim := c.SharedTasks() && (command == "finish-work" || command == "verify-claim" || command == "record-triage" || command == "link-task" || command == "record-work-result" || command == "prepare-worktree" || command == "verify-worktree" || command == "check-github-action" || command == "test-worktree" || command == "restart-sync" || command == "start-sync-batch")
	if c.RaftMode == "external" && (command == "doctor" || command == "serve" || command == "deliver" || command == "adopt-discussion" || needsClaim) {
		b, err := readSecret("GHRAFT_RAFT_TOKEN_FILE", c.PrivateFile("raft-token"))
		if err != nil {
			return err
		}
		raft.Credential = string(b)
	}
	svc := &app.Service{Config: c, Store: store, Raft: raft, GH: app.GH{Login: c.GitHubLogin, PrivateRepositories: c.PrivateRepositories, PrivateNotificationsSince: c.PrivateNotificationsSince, NotificationExcludeRepositories: c.NotificationExcludeRepositories}}
	switch command {
	case "cleanup":
		f := flag.NewFlagSet(command, flag.ContinueOnError)
		dryRun := f.Bool("dry-run", false, "Preview expired history and disposable state without changing stored data")
		if err := f.Parse(flags.Args()[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return errors.New("unexpected cleanup arguments")
		}
		stats, err := store.Cleanup(ctx, c.HistoryRetentionDays, *dryRun)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(stats)
	case "record-triage":
		f := flag.NewFlagSet(command, flag.ContinueOnError)
		var in app.Triage
		f.StringVar(&in.Executor, "executor", "", "Your own Raft @handle; required for shared tasks")
		f.StringVar(&in.EventID, "event", "", "Captured event ID")
		f.StringVar(&in.Outcome, "outcome", "", "quiet, notified or superseded")
		f.StringVar(&in.SupersededBy, "superseded-by", "", "Latest event whose report covers this older fork event")
		f.StringVar(&in.ReportID, "report", "", "Saved report UUID on the covering event; required for superseded")
		reasonFile := f.String("reason-file", "", "UTF-8 file containing the evidence-based reason")
		f.StringVar(&in.MessageID, "message", "", "Actual full notification message UUID; omit for quiet")
		f.StringVar(&in.Target, "target", "", "Configured human DM target; omit for quiet")
		if err := f.Parse(flags.Args()[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return errors.New("unexpected triage arguments")
		}
		reason, err := os.ReadFile(*reasonFile)
		if err != nil {
			return err
		}
		in.Reason = string(reason)
		if err = svc.RecordTriage(ctx, in); err != nil {
			if errors.Is(err, app.ErrClaimUnavailable) {
				return json.NewEncoder(os.Stdout).Encode(map[string]any{"recorded": false, "execute": false, "reason": "already_owned_or_finished"})
			}
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]bool{"recorded": true})
	case "doctor":
		checks, err := svc.Doctor(dbctx)
		if e := json.NewEncoder(os.Stdout).Encode(checks); e != nil {
			return e
		}
		return err
	case "finish-work", "verify-claim", "work-context", "refresh-work-context", "link-task", "record-work-result", "prepare-worktree", "verify-worktree", "discussion-candidates", "adopt-discussion", "check-github-action", "request-work", "test-worktree", "restart-sync", "start-sync-batch":
		return workCommand(ctx, svc, command, flags.Args()[1:])
	case "import-legacy":
		b, e := io.ReadAll(io.LimitReader(os.Stdin, 64<<20))
		if e != nil {
			return e
		}
		return store.ImportLegacy(ctx, c, b)
	case "status":
		v, e := store.Status(ctx)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(v)
	case "pending":
		v, e := svc.Pending(ctx)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(v)
	case "authorize-events":
		if c.RaftMode != "app" {
			return errors.New("external mode uses its credential directly; no App grant is needed")
		}
		return raft.AuthorizeEvents(dbctx)
	case "collect":
		c, done := context.WithTimeout(ctx, 10*time.Minute)
		defer done()
		return svc.Collect(c)
	case "deliver":
		return svc.Deliver(dbctx)
	case "events":
		events, e := svc.Events(dbctx)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(events)
	case "serve":
		// One collector per database. Local lock prevents accidental duplicate local processes.
		path := os.Getenv("GHRAFT_LOCK_FILE")
		if path == "" {
			path = c.PrivateFile("service.lock")
		}
		f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if e != nil {
			return e
		}
		defer f.Close()
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			return errors.New("another local service owns this instance")
		}
		if c.RaftMode == "external" {
			if _, err := raft.ExternalIdentity(dbctx); err != nil {
				return err
			}
			log.Printf("GitHub assistant %s started (external delivery, outbound connections only)", version)
			svc.Loop(ctx)
			return nil
		}
		server := &http.Server{Addr: c.Listen, Handler: svc.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
		go svc.Loop(ctx)
		go func() {
			<-ctx.Done()
			c, done := context.WithTimeout(context.Background(), 5*time.Second)
			defer done()
			server.Shutdown(c)
		}()
		log.Printf("GitHub Raft App listening on %s (GitHub read-only, authorized watches)", c.Listen)
		e = server.ListenAndServe()
		if errors.Is(e, http.ErrServerClosed) {
			return nil
		}
		return e
	default:
		return errors.New("commands: init, version, check-config, doctor, manifest, migrate, collect, events, status, pending, record-triage, authorize-events, deliver, serve, verify-claim, work-context, refresh-work-context, link-task, record-work-result, prepare-worktree, verify-worktree, discussion-candidates, adopt-discussion, check-github-action, request-work, test-worktree, restart-sync, start-sync-batch, finish-work")
	}
}

func workCommand(ctx context.Context, svc *app.Service, command string, args []string) error {
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	in := app.WorkInput{}
	f.StringVar(&in.Executor, "executor", "", "Your own Raft @handle; required for shared tasks")
	f.StringVar(&in.EventID, "event", "", "Captured event ID")
	f.StringVar(&in.EvidenceVersion, "version", "", "Current evidence version")
	f.StringVar(&in.GuideVersion, "guide", "", "Current guide version")
	f.StringVar(&in.TaskID, "task", "", "Existing claimed Raft task root UUID")
	after := f.String("after", "", "First batch baseline SHA; later batches resume the verified cursor")
	count := f.Int("count", 1, "Number of oldest next upstream first-parent commits, 1..50")
	repository := f.String("repo", "", "Existing local clone with the authorized GitHub origin")
	report := f.String("report", "", "Actual Raft report UUID")
	outcome := f.String("outcome", "", "investigated, changes-ready, needs-decision, waiting or merged")
	ref := f.String("ref", "", "Exact write branch for fork maintenance")
	action := f.String("action", "", "Delegated GitHub action; edit-pr checks title/body updates only")
	head := f.String("head", "", "Reviewed PR head SHA")
	base := f.String("base", "", "Reviewed PR base SHA")
	request := f.String("request", "", "Operator-supplied UUID; reuse for retries of the same work request")
	reason := f.String("reason-file", "", "UTF-8 operator work request reason")
	summaryFile := f.String("summary-file", "", "UTF-8 file containing result summary")
	bodyFile := f.String("body-file", "", "Proposed full PR body; required for adapted fork open-pr/edit-pr checks")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected work command arguments")
	}
	var result any
	var err error
	switch command {
	case "request-work":
		var body []byte
		body, err = os.ReadFile(*reason)
		if err == nil {
			result, err = svc.RequestWork(ctx, in.EventID, *request, string(body))
		}
	case "check-github-action":
		var body []byte
		if *bodyFile != "" {
			body, err = os.ReadFile(*bodyFile)
		}
		if err == nil {
			result, err = svc.CheckGitHubAction(ctx, app.GitHubActionInput{WorkInput: in, Action: *action, Head: *head, Base: *base, Ref: *ref, Body: string(body)})
		}
	case "test-worktree":
		if _, err = svc.RefreshWorkContext(ctx, in.EventID); err == nil {
			result, err = svc.TestWorkspace(ctx, in)
		}
	case "start-sync-batch":
		var body []byte
		body, err = os.ReadFile(*reason)
		if err == nil {
			result, err = svc.StartForkBatch(ctx, app.ForkBatchInput{WorkInput: in, After: *after, Count: *count, Reason: string(body)})
		}
	case "restart-sync":
		result, err = svc.RestartFork(ctx, in)
	case "finish-work":
		result, err = svc.FinishWork(ctx, in, func(ctx context.Context, task app.TaskDispatch, status string) error {
			cmd := exec.CommandContext(ctx, "raft", "task", "update", "--target", task.Channel, "--number", strconv.Itoa(task.Number), "--status", status)
			// Inherit the calling executor's identity and unread-message guard.
			// Do not substitute the collector's shared external credential.
			cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
			return cmd.Run()
		})
	case "adopt-discussion":
		result, err = svc.AdoptDiscussion(ctx, in.EventID, in.TaskID)
	case "discussion-candidates":
		var w app.WorkContext
		w, err = svc.WorkContext(ctx, in.EventID)
		if err == nil {
			result, err = svc.Store.DiscussionCandidates(ctx, w.Event.Key, svc.Config.DeliveryTarget)
		}
	case "verify-claim":
		if !svc.Config.SharedTasks() {
			return errors.New("verify-claim requires shared task delivery")
		}
		err = svc.VerifyClaim(ctx, in.EventID, in.Executor)
		if errors.Is(err, app.ErrClaimUnavailable) {
			result, err = map[string]any{"execute": false, "reason": "already_owned_or_finished"}, nil
		} else if err == nil {
			result = map[string]bool{"execute": true}
		}
	case "work-context":
		result, err = svc.WorkContext(ctx, in.EventID)
	case "refresh-work-context":
		result, err = svc.RefreshWorkContext(ctx, in.EventID)
	case "link-task":
		result, err = svc.LinkTask(ctx, in)
	case "record-work-result":
		var summary []byte
		summary, err = os.ReadFile(*summaryFile)
		if err == nil {
			err = svc.RecordWorkResult(ctx, app.WorkResult{WorkInput: in, ReportID: *report, Outcome: *outcome, Summary: string(summary)})
			result = map[string]bool{"recorded": err == nil}
		}
	case "prepare-worktree", "verify-worktree":
		if _, err = svc.RefreshWorkContext(ctx, in.EventID); err != nil {
			return err
		}
		if command == "prepare-worktree" {
			if *repository == "" {
				return errors.New("--repo is required")
			}
			result, err = svc.PrepareWorkspace(ctx, in, *repository)
		} else {
			result, err = svc.VerifyWorkspace(ctx, in)
		}
	}
	if errors.Is(err, app.ErrClaimUnavailable) {
		result, err = map[string]any{"execute": false, "reason": "already_owned_or_finished"}, nil
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
func main() {
	if e := run(); e != nil {
		log.Print(e)
		os.Exit(1)
	}
}
