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
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github-raft-app/internal/app"
)

func readSecret(env string) ([]byte, error) {
	p := os.Getenv(env)
	if p == "" {
		return nil, fmt.Errorf("%s is required", env)
	}
	i, e := os.Stat(p)
	if e != nil || !i.Mode().IsRegular() || i.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("%s must name a private regular file (mode 0600)", env)
	}
	b, e := os.ReadFile(p)
	return []byte(strings.TrimSpace(string(b))), e
}
func run() error {
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
	if command == "manifest" {
		s := app.Service{Config: c}
		return json.NewEncoder(os.Stdout).Encode(s.Manifest())
	}
	dsn, e := readSecret("GHRAFT_DSN_FILE")
	if e != nil {
		return e
	}
	raw, e := readSecret("GHRAFT_KEY_FILE")
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
	store, e := app.OpenStore(dbctx, string(dsn), key)
	if e != nil {
		return e
	}
	defer store.DB.Close()
	store.LookbackHours = c.LookbackHours
	if command == "migrate" {
		return store.Migrate(dbctx)
	}
	secret := ""
	if os.Getenv("GHRAFT_CLIENT_SECRET_FILE") != "" {
		b, e := readSecret("GHRAFT_CLIENT_SECRET_FILE")
		if e != nil {
			return e
		}
		secret = string(b)
	}
	raft := &app.Raft{Config: c, Secret: secret, HTTP: app.SafeHTTP(), Store: store}
	svc := &app.Service{Config: c, Store: store, Raft: raft, GH: app.GH{Login: c.GitHubLogin, Config: c}}
	switch command {
	case "work-context", "refresh-work-context", "link-task", "record-work-result", "prepare-worktree", "verify-worktree":
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
		return raft.AuthorizeEvents(dbctx)
	case "collect":
		c, done := context.WithTimeout(ctx, 10*time.Minute)
		defer done()
		return svc.Collect(c)
	case "deliver":
		return svc.Deliver(dbctx)
	case "events":
		events, e := store.Events(dbctx, false)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(events)
	case "serve":
		// One collector per database. Local lock prevents accidental duplicate local processes.
		path := os.Getenv("GHRAFT_LOCK_FILE")
		if path == "" {
			return errors.New("GHRAFT_LOCK_FILE required for the single-instance service")
		}
		f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if e != nil {
			return e
		}
		defer f.Close()
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			return errors.New("another local service owns this instance")
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
		return errors.New("commands: manifest, migrate, collect, events, status, pending, authorize-events, deliver, serve, work-context, refresh-work-context, link-task, record-work-result, prepare-worktree, verify-worktree")
	}
}

func workCommand(ctx context.Context, svc *app.Service, command string, args []string) error {
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	in := app.WorkInput{}
	f.StringVar(&in.EventID, "event", "", "Captured event ID")
	f.StringVar(&in.EvidenceVersion, "version", "", "Current evidence version")
	f.StringVar(&in.GuideVersion, "guide", "", "Current guide version")
	f.StringVar(&in.TaskID, "task", "", "Existing claimed Raft task root UUID")
	repository := f.String("repo", "", "Existing local clone with the authorized GitHub origin")
	report := f.String("report", "", "Actual Raft report UUID")
	outcome := f.String("outcome", "", "investigated, changes-ready or needs-decision")
	summaryFile := f.String("summary-file", "", "UTF-8 file containing result summary")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected work command arguments")
	}
	var result any
	var err error
	switch command {
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
