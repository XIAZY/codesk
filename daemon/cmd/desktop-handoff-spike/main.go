package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"notty/daemon/internal/desktop/handoff"
)

const (
	connectPagePath = "/desktop-handoff-spike.html"
	maxTimeout      = 30 * time.Minute
)

type handoffSession interface {
	CallbackURL() string
	Wait(context.Context) (handoff.Payload, error)
	Close() error
}

type sessionFactory func(string) (handoffSession, error)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, func(codeskOrigin string) (handoffSession, error) {
		return handoff.NewSession(codeskOrigin)
	}))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, newSession sessionFactory) int {
	flags := flag.NewFlagSet("desktop-handoff-spike", flag.ContinueOnError)
	flags.SetOutput(stderr)
	connectPageValue := flags.String("connect-page", "", "HTTPS Codesk handoff harness URL")
	timeout := flags.Duration("timeout", 5*time.Minute, "maximum time to wait for the browser handoff")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "desktop handoff: unexpected positional arguments")
		return 2
	}
	connectPage, err := parseConnectPage(*connectPageValue)
	if err != nil {
		fmt.Fprintf(stderr, "desktop handoff: --connect-page must be an absolute HTTP(S) Codesk URL at %s without credentials, query, or fragment\n", connectPagePath)
		return 2
	}
	if *timeout <= 0 || *timeout > maxTimeout {
		fmt.Fprintf(stderr, "desktop handoff: --timeout must be greater than zero and no more than %s\n", maxTimeout)
		return 2
	}

	session, err := newSession(connectPageOrigin(connectPage))
	if err != nil {
		fmt.Fprintln(stderr, "desktop handoff: could not start the loopback receiver")
		return 1
	}
	defer session.Close()

	launchURL := connectPageWithCallback(connectPage, session.CallbackURL())
	fmt.Fprintf(stdout, "connect_url=%s\n", launchURL)
	fmt.Fprintln(stdout, "waiting_for_browser=true")

	waitCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	payload, err := session.Wait(waitCtx)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			fmt.Fprintln(stderr, "desktop handoff: timed out waiting for the browser")
		case errors.Is(err, context.Canceled):
			fmt.Fprintln(stderr, "desktop handoff: canceled")
		default:
			fmt.Fprintln(stderr, "desktop handoff: receiver failed")
		}
		return 1
	}

	fmt.Fprintln(stdout, "handoff_accepted=true")
	fmt.Fprintf(stdout, "daemon_id=%s\n", payload.DaemonID)
	fmt.Fprintf(stdout, "workspace_id=%s\n", payload.WorkspaceID)
	fmt.Fprintln(stdout, "token_received=true")
	return 0
}

func parseConnectPage(raw string) (*url.URL, error) {
	if raw == "" || strings.Contains(raw, "#") {
		return nil, errors.New("empty connect page")
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("invalid connect page")
	}
	if parsed.User != nil || parsed.Path != connectPagePath || parsed.EscapedPath() != connectPagePath ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.String() != raw {
		return nil, errors.New("connect page contains forbidden URL components")
	}
	if parsed.Scheme == "http" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost" {
		return nil, errors.New("remote connect page must use HTTPS")
	}
	if !validConnectPagePort(parsed) {
		return nil, errors.New("connect page contains an invalid port")
	}
	return parsed, nil
}

func validConnectPagePort(value *url.URL) bool {
	port := value.Port()
	if port == "" {
		return !strings.HasSuffix(value.Host, ":")
	}
	number, err := strconv.Atoi(port)
	return err == nil && number > 0 && number <= 65535
}

func connectPageOrigin(connectPage *url.URL) string {
	return (&url.URL{Scheme: connectPage.Scheme, Host: connectPage.Host}).String()
}

func connectPageWithCallback(connectPage *url.URL, callback string) string {
	launchURL := *connectPage
	query := launchURL.Query()
	query.Set("callback", callback)
	launchURL.RawQuery = query.Encode()
	return launchURL.String()
}
