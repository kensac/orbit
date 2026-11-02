package build

import (
	"fmt"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// maxCommand bounds a Bash command orbit will parse. The commands a build
// needs are one line; anything this long is not one of them.
const maxCommand = 8 << 10

// bash allows a command line only when a shell parser (Bash's grammar, the
// shell the CLI runs commands with) reads it as simple commands joined by
// &&, ||, ; or |, every word a literal, every command's argv starting with
// an allowed prefix, and every redirection to /dev/null or a writable file
// in the checkout. No expansion of any kind is allowed, so the argv orbit
// checks is the argv that runs.
func (p Policy) bash(command string) Decision {
	if strings.TrimSpace(command) == "" {
		return deny("empty command")
	}
	if len(command) > maxCommand {
		return deny("command is longer than %d bytes", maxCommand)
	}
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return p.shellDenial("orbit could not parse it as a shell command")
	}
	if len(f.Stmts) == 0 {
		return deny("empty command")
	}
	for _, st := range f.Stmts {
		if why := p.stmt(st); why != "" {
			return p.shellDenial(why)
		}
	}
	return allow
}

func (p Policy) shellDenial(why string) Decision {
	return deny("%s. A build may run only these commands (and their arguments): %s; "+
		"joined by &&, ||, ; or |, from %s, with literal arguments only: no $variables, $(...), backticks, "+
		"globs in the command, subshells, functions, background jobs or here-documents, and redirections "+
		"only to /dev/null or files inside %s. Run each command on its own if in doubt.",
		why, p.commandList(), p.root(), p.root())
}

// stmt returns why one statement is refused, or "".
func (p Policy) stmt(st *syntax.Stmt) string {
	if st == nil || st.Cmd == nil {
		return "a statement with no command"
	}
	if st.Background || st.Coprocess || st.Disown {
		return "background jobs are not allowed"
	}
	for _, r := range st.Redirs {
		if why := p.redirect(r); why != "" {
			return why
		}
	}
	switch c := st.Cmd.(type) {
	case *syntax.BinaryCmd:
		if c.Op != syntax.AndStmt && c.Op != syntax.OrStmt && c.Op != syntax.Pipe {
			return fmt.Sprintf("the %s operator is not allowed", c.Op)
		}
		if why := p.stmt(c.X); why != "" {
			return why
		}
		return p.stmt(c.Y)
	case *syntax.CallExpr:
		if len(c.Assigns) > 0 {
			return "environment assignments are not allowed"
		}
		if len(c.Args) == 0 {
			return "a statement with no command"
		}
		argv := make([]string, 0, len(c.Args))
		for _, w := range c.Args {
			s, ok := literal(w)
			if !ok {
				return fmt.Sprintf("%q is not a literal word", wordSource(w))
			}
			argv = append(argv, s)
		}
		if !p.allowedArgv(argv) {
			return fmt.Sprintf("`%s` is not an allowed command", strings.Join(argv, " "))
		}
		return ""
	}
	return "only simple commands are allowed"
}

func (p Policy) allowedArgv(argv []string) bool {
	for _, prefix := range p.Commands {
		if len(prefix) > 0 && len(argv) >= len(prefix) && slices.Equal(argv[:len(prefix)], prefix) {
			return true
		}
	}
	return false
}

// redirect returns why a redirection is refused, or "".
func (p Policy) redirect(r *syntax.Redirect) string {
	if r.N != nil && strings.Trim(r.N.Value, "0123456789") != "" {
		return "only numbered file descriptors may be redirected"
	}
	target, ok := literal(r.Word)
	if !ok || strings.ContainsAny(target, "*?[]{}") {
		return "a redirection target must be a literal path"
	}
	switch r.Op {
	case syntax.DplOut, syntax.DplIn:
		// 2>&1, >&2, <&0, and closing one with -.
		if target == "-" || strings.Trim(target, "0123456789") == "" {
			return ""
		}
		return "only file descriptors may be duplicated"
	case syntax.RdrOut, syntax.AppOut, syntax.RdrClob, syntax.RdrAll, syntax.AppAll:
		if target == "/dev/null" {
			return ""
		}
		if d := p.writable(target); !d.Allow {
			return "redirection to " + target + ": " + d.Message
		}
		return ""
	case syntax.RdrIn:
		if target == "/dev/null" || p.inside(target) {
			return ""
		}
		return "redirection from " + target + " is outside " + p.root()
	}
	return fmt.Sprintf("the %s redirection is not allowed", r.Op)
}

// literal returns a word's value when it is made only of literal text and
// quotes: no parameter, command, arithmetic or process substitution, no
// backslash escapes or tilde to interpret, no locale or ANSI-C quoting.
func literal(w *syntax.Word) (string, bool) {
	if w == nil || len(w.Parts) == 0 {
		return "", false
	}
	var b strings.Builder
	for i, part := range w.Parts {
		switch x := part.(type) {
		case *syntax.Lit:
			if strings.ContainsAny(x.Value, "\\$`") || (i == 0 && strings.HasPrefix(x.Value, "~")) {
				return "", false
			}
			b.WriteString(x.Value)
		case *syntax.SglQuoted:
			if x.Dollar {
				return "", false
			}
			b.WriteString(x.Value)
		case *syntax.DblQuoted:
			if x.Dollar {
				return "", false
			}
			for _, q := range x.Parts {
				l, ok := q.(*syntax.Lit)
				if !ok || strings.ContainsAny(l.Value, "\\$`") {
					return "", false
				}
				b.WriteString(l.Value)
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}

func wordSource(w *syntax.Word) string {
	var b strings.Builder
	_ = syntax.NewPrinter().Print(&b, w)
	s := b.String()
	if len(s) > 80 {
		s = s[:80] + "..."
	}
	return s
}
