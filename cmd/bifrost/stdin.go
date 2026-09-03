package main

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strings"
)

// readLineFromStdin reads one trimmed, non-empty line from stdin — for
// piped secrets (--password-stdin, --subject-token-stdin), ported from
// the predecessor CLI's read_line_from_stdin.
func readLineFromStdin() (string, error) {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", errors.New("empty input on stdin")
	}
	return line, nil
}
