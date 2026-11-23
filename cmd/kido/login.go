package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"golang.org/x/term"

	"github.com/kido-sh/kido/internal/registry"
)

func init() {
	register("login", "Log in to an OCI registry", regLogin)
}

func regLogin(ctx context.Context, args []string) int {
	fs := ctrFlags("login", "[OPTIONS] [SERVER]\n\n"+
		"Checks the credentials against SERVER (default docker.io) and stores them in the credential\n"+
		"helper that ~/.docker/config.json names, or in that file.")
	username := fs.String("u", "", "username")
	fs.StringVar(username, "username", "", "username")
	passwordStdin := fs.Bool("password-stdin", false, "read the password from stdin")
	if err := fs.Parse(ctrInterspersed(fs, args)); err != nil {
		return 125
	}
	pos := fs.Args()
	if len(pos) > 1 {
		fs.Usage()
		return 125
	}
	server := ""
	if len(pos) == 1 {
		server = pos[0]
	}
	stdinTTY := term.IsTerminal(int(os.Stdin.Fd()))
	stdin := bufio.NewReader(os.Stdin)
	if *username == "" {
		if !stdinTTY || *passwordStdin {
			fmt.Fprintln(os.Stderr, "kido login: -u is required when stdin is not a terminal")
			return 125
		}
		fmt.Fprint(os.Stderr, "Username: ")
		line, err := stdin.ReadString('\n')
		if err != nil && err != io.EOF {
			fmt.Fprintln(os.Stderr, "kido login:", err)
			return 125
		}
		*username = strings.TrimSpace(line)
	}
	var password string
	if *passwordStdin {
		b, err := io.ReadAll(io.LimitReader(stdin, 64<<10))
		if err != nil {
			fmt.Fprintln(os.Stderr, "kido login:", err)
			return 125
		}
		password = strings.TrimRight(string(b), "\r\n")
	} else {
		if !stdinTTY {
			fmt.Fprintln(os.Stderr, "kido login: stdin is not a terminal; use --password-stdin")
			return 125
		}
		fmt.Fprint(os.Stderr, "Password: ")
		b, err := regReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			fmt.Fprintln(os.Stderr, "kido login:", err)
			return 125
		}
		password = string(b)
	}
	where, err := registry.Login(ctx, server, *username, password)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kido login:", registry.Sanitize(err.Error()))
		return 125
	}
	fmt.Fprintln(os.Stderr, "Credentials stored in", where)
	fmt.Println("Login Succeeded")
	return 0
}

// regReadPassword reads a password without echo and turns echo back on if Ctrl-C ends the
// program at the prompt (main catches only SIGTERM).
func regReadPassword(fd int) ([]byte, error) {
	state, err := term.GetState(fd)
	if err != nil {
		return nil, err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	done := make(chan struct{})
	defer func() {
		signal.Stop(sig)
		close(done)
	}()
	go func() {
		select {
		case <-sig:
			term.Restore(fd, state)
			fmt.Fprintln(os.Stderr)
			os.Exit(130)
		case <-done:
		}
	}()
	return term.ReadPassword(fd)
}
