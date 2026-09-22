package mailer

import (
	"sort"
	"strings"
	textt "text/template"
	"text/template/parse"
)

// ReferencedVariables returns the fields of the data the mailer passes an
// HTML body — a send's variables, Email and FullName — that the body
// references, each once, sorted. It is what the Required variable check
// counts, so it reads a body the way the mailer does: parsed as a Go template
// with the mailer's functions, not searched as text. A body the mailer could
// not parse is an error, the one the mailer would meet.
//
// A reference is a field of the mailer's data used in an action:
//
//   - .X and $.X, in a plain action, an if or else if condition, a function's
//     arguments or a pipeline; of a chain only the first field (.Event of
//     .Event.Title) counts;
//   - inside range and with, .X is a field of the element and does not count,
//     $.X still does; their else branch runs with the outer dot again;
//   - index . "X", a bare dot, a variable's fields ($e.Title), a comment, a
//     string and the text around the actions do not count;
//   - nothing inside a define counts, neither .X nor $.X: whoever calls it
//     decides its data. A block given dot where dot is the mailer's data reads
//     like the text around it; any other block is read like a define.
//
// skymail-frontend's render module counts with the same rule (its
// go-template.ts), and the two are held to the same cases in
// testdata/referenced-variables.json.
func ReferencedVariables(html string) ([]string, error) {
	tmpl, err := textt.New("html").Funcs(mailFuncs).Parse(html)
	if err != nil {
		return nil, err
	}

	walk := variableWalk{tmpl: tmpl, text: html, found: map[string]struct{}{}, inBlock: map[string]bool{}}
	if tmpl.Tree != nil {
		walk.node(tmpl.Tree.Root, mailerData)
	}

	names := make([]string, 0, len(walk.found))
	for name := range walk.found {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// scope says what the two ways of reaching data read at a point of a body.
type scope struct {
	// Whether dot is the mailer's data; range and with rebind it.
	dot bool
	// Whether $ is; inside a define or a block, $ is its caller's argument.
	dollar bool
}

var mailerData = scope{dot: true, dollar: true}

type variableWalk struct {
	tmpl  *textt.Template
	text  string
	found map[string]struct{}
	// The blocks being read, so a block that calls itself is read once.
	inBlock map[string]bool
}

func (w *variableWalk) node(node parse.Node, s scope) {
	switch n := node.(type) {
	case *parse.ListNode:
		if n == nil {
			return
		}
		for _, child := range n.Nodes {
			w.node(child, s)
		}
	case *parse.ActionNode:
		w.node(n.Pipe, s)
	case *parse.IfNode:
		w.node(n.Pipe, s)
		w.node(n.List, s)
		w.node(n.ElseList, s)
	case *parse.RangeNode:
		w.branch(&n.BranchNode, s)
	case *parse.WithNode:
		w.branch(&n.BranchNode, s)
	case *parse.TemplateNode:
		w.node(n.Pipe, s)
		w.block(n, s)
	case *parse.PipeNode:
		if n == nil {
			return
		}
		// Declarations ($x :=) are the names being bound, not references.
		for _, command := range n.Cmds {
			w.node(command, s)
		}
	case *parse.CommandNode:
		for _, arg := range n.Args {
			w.node(arg, s)
		}
	case *parse.ChainNode:
		// (pipeline).Field: the pipeline's references; the field is the result's.
		w.node(n.Node, s)
	case *parse.FieldNode:
		if s.dot {
			w.found[n.Ident[0]] = struct{}{}
		}
	case *parse.VariableNode:
		if s.dollar && n.Ident[0] == "$" && len(n.Ident) > 1 {
			w.found[n.Ident[1]] = struct{}{}
		}
	}
}

// branch reads a range or a with: its value where it stands, its body with
// dot rebound to the element, its else branch where it stands.
func (w *variableWalk) branch(b *parse.BranchNode, s scope) {
	w.node(b.Pipe, s)
	w.node(b.List, scope{dot: false, dollar: s.dollar})
	w.node(b.ElseList, s)
}

// block reads the body of a {{block "name" .}} given the mailer's data as if
// it stood where the block does. A {{template}} call is not followed: the
// body it calls is a define, and nothing inside a define counts.
func (w *variableWalk) block(n *parse.TemplateNode, s scope) {
	if !s.dot || !givenDot(n.Pipe) || !w.isBlock(n) || w.inBlock[n.Name] {
		return
	}
	called := w.tmpl.Lookup(n.Name)
	if called == nil || called.Tree == nil {
		return
	}
	w.inBlock[n.Name] = true
	w.node(called.Tree.Root, mailerData)
	w.inBlock[n.Name] = false
}

// isBlock tells a block from a template call, which parse to the same node:
// the node sits at the block's name, right after the keyword that opened it.
func (w *variableWalk) isBlock(n *parse.TemplateNode) bool {
	at := int(n.Pos)
	if at > len(w.text) {
		return false
	}
	return strings.HasSuffix(strings.TrimRight(w.text[:at], " \t\r\n"), "block")
}

// givenDot reports whether a pipeline is dot and nothing else.
func givenDot(pipe *parse.PipeNode) bool {
	if pipe == nil || len(pipe.Decl) > 0 || len(pipe.Cmds) != 1 || len(pipe.Cmds[0].Args) != 1 {
		return false
	}
	_, isDot := pipe.Cmds[0].Args[0].(*parse.DotNode)
	return isDot
}
