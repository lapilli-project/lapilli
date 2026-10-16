# Contributing

```sh
go build .            # the lapilli binary
go test ./...         # needs ssh-keygen on PATH
gofmt -l . && go vet ./...
```

Every rule in `internal/diagnose` is pinned to a real `ssh -vvv` log in
`testdata/logs`, and `TestSavedLogs` says which conclusion each log must reach.
To teach lapilli a new failure:

1. Reproduce it. `scripts/lab` starts local sshd instances with throwaway keys
   and captures the client's log; a log from a real server works too.
2. Replace local paths and user names with `/home/alice/.ssh` and `alice`, and
   add the log to `testdata/logs` (and a line to `testdata/README.md` if it was
   written by hand rather than captured).
3. Add it to the tables in `internal/sshlog/sshlog_test.go` and
   `internal/diagnose/diagnose_test.go`, then change the rule until both pass.

A log from an OpenSSH version not yet in `testdata/logs` is welcome on its own,
even without a fix.
