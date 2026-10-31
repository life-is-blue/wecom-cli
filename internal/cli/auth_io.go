package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/life-is-blue/openetl-cli/internal/cli/credentials"
)

func credentialsOf(base, project, token string) credentials.Credentials {
	return credentials.Credentials{BaseURL: base, Project: project, Token: token}
}

func asCLI(err error, target **cliError) bool { return errors.As(err, target) }

func (a *App) readToken(fromStdin bool) (string, error) {
	if !fromStdin {
		if f, ok := a.Stdin.(*os.File); ok {
			if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
				fmt.Fprint(a.Stderr, "API token (input is visible; prefer --token-stdin): ")
			}
		}
	}
	line, err := bufio.NewReader(a.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", runtimef("read token: %v", err)
	}
	tok := strings.TrimSpace(line)
	if tok == "" {
		return "", usagef("no token received on stdin")
	}
	return tok, nil
}
