package app

import (
	"context"
	"errors"
	"os/exec"
)

type Diagnostic struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Doctor performs read-only checks. It never migrates, collects, posts a test
// message, or requests new authorization as a side effect.
func (s *Service) Doctor(ctx context.Context) ([]Diagnostic, error) {
	checks := []Diagnostic{}
	add := func(name, success string, err error) {
		d := Diagnostic{Name: name, OK: err == nil, Detail: success}
		if err != nil {
			d.Detail = err.Error()
		}
		checks = append(checks, d)
	}
	_, err := exec.LookPath("gh")
	add("github_cli", "gh is installed", err)
	if err == nil {
		add("github_identity", "configured GitHub account is authenticated", s.GH.CheckIdentity(ctx))
	}
	add("database", "database connection is available", s.Store.Ping(ctx))
	err = s.Store.SchemaReady(ctx)
	if err != nil {
		err = errors.New("database schema is unavailable; run migrate first")
	}
	add("schema", "database schema is initialized", err)
	if s.Config.RaftMode == "external" {
		_, err = s.Raft.ExternalIdentity(ctx)
		add("raft_identity", "external identity, server and read/send capabilities match", err)
	} else {
		add("raft_identity", "", errors.New("App mode requires its existing OAuth login; doctor does not request authorization"))
	}
	for _, check := range checks {
		if !check.OK {
			return checks, errors.New("deployment diagnostics failed")
		}
	}
	return checks, nil
}
