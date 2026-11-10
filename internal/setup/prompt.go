package setup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// errInputClosed is returned when the input ends before the operator has
// answered every prompt. Setup treats it as a cancellation: nothing is
// written.
var errInputClosed = errors.New("setup: input closed before the configuration was complete; nothing was written")

// prompter reads answers from a stream and writes prompts to another.
//
// It is deliberately plain: no terminal control, no history, no line
// editing. That keeps it usable over a pipe, in a test, and on every
// platform, and it means the setup flow can be scripted deterministically.
type prompter struct {
	in  *bufio.Reader
	out io.Writer
	// exhausted records that the input has ended. Once it is set, every
	// further read fails, so a validation loop cannot spin forever.
	exhausted bool
}

func newPrompter(in io.Reader, out io.Writer) *prompter {
	return &prompter{in: bufio.NewReader(in), out: out}
}

// readLine reads one line, bounded by the context.
//
// A final line without a trailing newline is accepted. An end of input
// with no text left is reported as errInputClosed.
//
// The read itself is a blocking call on the underlying reader, so it runs
// in one goroutine and the caller waits on either the result or the
// context. When the context ends first the prompter is marked exhausted,
// so no second read is ever started on the same reader; the abandoned
// goroutine is bounded by the process that owns the reader, which for the
// setup command exits immediately afterwards.
func (p *prompter) readLine(ctx context.Context) (string, error) {
	if p.exhausted {
		return "", errInputClosed
	}

	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		text, err := p.in.ReadString('\n')
		done <- result{text: text, err: err}
	}()

	select {
	case <-ctx.Done():
		p.exhausted = true
		return "", ctx.Err()
	case r := <-done:
		if r.err != nil {
			if errors.Is(r.err, io.EOF) {
				p.exhausted = true
				if r.text == "" {
					return "", errInputClosed
				}
				return r.text, nil
			}
			return "", fmt.Errorf("setup: read input: %w", r.err)
		}
		return r.text, nil
	}
}

// ask prompts for a value, applying a default when the answer is blank.
//
// The default is shown in the prompt. A required value cannot be skipped:
// the prompt repeats until the operator supplies one, or the input ends.
func (p *prompter) ask(ctx context.Context, label, def string, required bool) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if def != "" {
			fmt.Fprintf(p.out, "%s [%s]: ", label, def)
		} else {
			fmt.Fprintf(p.out, "%s: ", label)
		}
		text, err := p.readLine(ctx)
		if err != nil {
			return "", err
		}
		value := strings.TrimSpace(text)
		if value == "" {
			value = def
		}
		if value == "" && required {
			fmt.Fprintln(p.out, "  a value is required")
			continue
		}
		return value, nil
	}
}

// askPort prompts for a TCP port.
func (p *prompter) askPort(ctx context.Context, label string, def int) (int, error) {
	for {
		raw, err := p.ask(ctx, label, strconv.Itoa(def), true)
		if err != nil {
			return 0, err
		}
		port, convErr := strconv.Atoi(raw)
		if convErr != nil || port < 1 || port > 65535 {
			fmt.Fprintln(p.out, "  enter a port number between 1 and 65535")
			continue
		}
		return port, nil
	}
}

// askYesNo prompts for a yes or no answer.
func (p *prompter) askYesNo(ctx context.Context, label string, def bool) (bool, error) {
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	for {
		raw, err := p.ask(ctx, label+" "+hint, "", false)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(raw) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			fmt.Fprintln(p.out, "  answer y or n")
		}
	}
}

// askCommand prompts for a credential command and splits it into an
// argument vector.
//
// The command is never echoed back: only its executable name is ever
// printed, because its arguments can name where the secret lives.
func (p *prompter) askCommand(ctx context.Context) ([]string, error) {
	for {
		raw, err := p.ask(ctx, "Credential command (e.g. pass show mail/work)", "", true)
		if err != nil {
			return nil, err
		}
		args, splitErr := SplitCommand(raw)
		if splitErr != nil {
			fmt.Fprintf(p.out, "  %v\n", splitErr)
			continue
		}
		return args, nil
	}
}

// SplitCommand splits a command line into an argument vector.
//
// It is not a shell. There is no variable expansion, globbing, command
// substitution or redirection. Single quotes preserve everything
// literally, double quotes honour a backslash escape, and a backslash
// outside quotes escapes the next character. The resulting vector is run
// directly by the credential resolver, never through a shell.
func SplitCommand(line string) ([]string, error) {
	var (
		args    []string
		word    strings.Builder
		inWord  bool
		quote   rune
		escaped bool
	)
	for _, r := range line {
		switch {
		case escaped:
			word.WriteRune(r)
			inWord = true
			escaped = false
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				word.WriteRune(r)
			}
		case quote == '"':
			switch r {
			case '"':
				quote = 0
			case '\\':
				escaped = true
			default:
				word.WriteRune(r)
			}
		case r == '\\':
			escaped = true
			inWord = true
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				args = append(args, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	if escaped {
		return nil, errors.New("the command ends with a backslash")
	}
	if quote != 0 {
		return nil, errors.New("the command has an unterminated quote")
	}
	if inWord {
		args = append(args, word.String())
	}
	if len(args) == 0 {
		return nil, errors.New("the command is empty")
	}
	return args, nil
}

// askChoice prompts for one of a small set of numbered options.
//
// The number is prompted rather than the label so the answer is
// unambiguous on every platform and every terminal.
func (p *prompter) askChoice(ctx context.Context, label string, options []string, def int) (int, error) {
	var b strings.Builder
	b.WriteString(label)
	b.WriteString(" [")
	for i, opt := range options {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%d=%s", i+1, opt)
	}
	b.WriteString("]")
	prompt := b.String()

	for {
		raw, err := p.ask(ctx, prompt, strconv.Itoa(def), true)
		if err != nil {
			return 0, err
		}
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n < 1 || n > len(options) {
			fmt.Fprintf(p.out, "  enter a number between 1 and %d\n", len(options))
			continue
		}
		return n - 1, nil
	}
}
