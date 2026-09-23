package mailer

import (
	"bytes"
	"math"
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
//   - an action inside an HTML comment does not count: html/template leaves
//     the comment, and whatever its actions print, out of the mail. A comment
//     runs from <!-- in the text between actions to the next -->, as
//     html/template reads it in text; the actions in it still open and close
//     blocks;
//   - nothing inside a define counts, neither .X nor $.X: whoever calls it
//     decides its data. A block given dot where dot is the mailer's data reads
//     like the text around it; any other block is read like a define.
//
// skymail-frontend's render module counts with the same rule (its
// go-template.ts), and the two are held to the same cases in
// testdata/referenced-variables.json.
func ReferencedVariables(html string) ([]string, error) {
	tmpl, err := textt.New("html").Funcs(htmlFuncs).Parse(html)
	if err != nil {
		return nil, err
	}

	walk := variableWalk{
		tmpl:     tmpl,
		text:     html,
		comments: htmlComments(tmpl),
		found:    map[string]struct{}{},
		inBlock:  map[string]bool{},
	}
	if tmpl.Tree != nil {
		walk.node(tmpl.Tree.Root, true)
	}

	names := make([]string, 0, len(walk.found))
	for name := range walk.found {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// The walk reads only the body the mailer executes and the blocks given its
// data, so $ is the mailer's data wherever the walk goes. Dot is too, except
// inside range and with: the walk carries whether it is.
type variableWalk struct {
	tmpl *textt.Template
	text string
	// The spans of the body inside HTML comments, in order.
	comments []span
	found    map[string]struct{}
	// The blocks being read, so a block that calls itself is read once.
	inBlock map[string]bool
}

// span is a stretch of the body, [start, end) in bytes.
type span struct{ start, end int }

// reference counts name, found at pos, unless an HTML comment hides it.
func (w *variableWalk) reference(name string, pos parse.Pos) {
	for _, comment := range w.comments {
		if int(pos) >= comment.start && int(pos) < comment.end {
			return
		}
	}
	w.found[name] = struct{}{}
}

func (w *variableWalk) node(node parse.Node, dot bool) {
	switch n := node.(type) {
	case *parse.ListNode:
		if n == nil {
			return
		}
		for _, child := range n.Nodes {
			w.node(child, dot)
		}
	case *parse.ActionNode:
		w.node(n.Pipe, dot)
	case *parse.IfNode:
		w.node(n.Pipe, dot)
		w.node(n.List, dot)
		w.node(n.ElseList, dot)
	case *parse.RangeNode:
		w.branch(&n.BranchNode, dot)
	case *parse.WithNode:
		w.branch(&n.BranchNode, dot)
	case *parse.TemplateNode:
		w.node(n.Pipe, dot)
		w.block(n, dot)
	case *parse.PipeNode:
		if n == nil {
			return
		}
		// Declarations ($x :=) are the names being bound, not references.
		for _, command := range n.Cmds {
			w.node(command, dot)
		}
	case *parse.CommandNode:
		for _, arg := range n.Args {
			w.node(arg, dot)
		}
	case *parse.ChainNode:
		// (pipeline).Field: the pipeline's references; the field is the result's.
		w.node(n.Node, dot)
	case *parse.FieldNode:
		if dot {
			w.reference(n.Ident[0], n.Pos)
		}
	case *parse.VariableNode:
		if n.Ident[0] == "$" && len(n.Ident) > 1 {
			w.reference(n.Ident[1], n.Pos)
		}
	}
}

// branch reads a range or a with: its value where it stands, its body with
// dot rebound to the element, its else branch where it stands.
func (w *variableWalk) branch(b *parse.BranchNode, dot bool) {
	w.node(b.Pipe, dot)
	w.node(b.List, false)
	w.node(b.ElseList, dot)
}

// block reads the body of a {{block "name" .}} given the mailer's data as if
// it stood where the block does. A {{template}} call is not followed: the
// body it calls is a define, and nothing inside a define counts.
func (w *variableWalk) block(n *parse.TemplateNode, dot bool) {
	if !dot || !givenDot(n.Pipe) || !w.isBlock(n) || w.inBlock[n.Name] {
		return
	}
	called := w.tmpl.Lookup(n.Name)
	if called == nil || called.Tree == nil {
		return
	}
	w.inBlock[n.Name] = true
	w.node(called.Tree.Root, true)
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

// htmlComments finds the spans of a body inside HTML comments, the way
// html/template does in text: <!-- opens one and the next --> after it closes
// it, even across actions. Only the text between actions is read — every
// template's text nodes, in the order they stand in the body — so a <!-- in
// an action's string opens nothing. html/template reads <!-- the same way
// only in text, not in a tag, a <script> or a <style>; taking one there for a
// comment can only make a reference not count, never make a missing one count.
func htmlComments(tmpl *textt.Template) []span {
	var texts []*parse.TextNode
	for _, t := range tmpl.Templates() {
		if t.Tree != nil {
			texts = appendTexts(texts, t.Tree.Root)
		}
	}
	sort.Slice(texts, func(i, j int) bool { return texts[i].Pos < texts[j].Pos })

	var comments []span
	open := -1
	for _, text := range texts {
		at := 0
		for {
			if open < 0 {
				i := bytes.Index(text.Text[at:], commentStart)
				if i < 0 {
					break
				}
				at += i + len(commentStart)
				open = int(text.Pos) + at - len(commentStart)
				continue
			}
			i := bytes.Index(text.Text[at:], commentEnd)
			if i < 0 {
				break
			}
			at += i + len(commentEnd)
			comments = append(comments, span{start: open, end: int(text.Pos) + at})
			open = -1
		}
	}
	if open >= 0 {
		comments = append(comments, span{start: open, end: math.MaxInt})
	}
	return comments
}

var (
	commentStart = []byte("<!--")
	commentEnd   = []byte("-->")
)

// appendTexts collects the text nodes under node, branches included.
func appendTexts(texts []*parse.TextNode, node parse.Node) []*parse.TextNode {
	switch n := node.(type) {
	case *parse.TextNode:
		return append(texts, n)
	case *parse.ListNode:
		if n == nil {
			return texts
		}
		for _, child := range n.Nodes {
			texts = appendTexts(texts, child)
		}
	case *parse.IfNode:
		texts = appendTexts(appendTexts(texts, n.List), n.ElseList)
	case *parse.RangeNode:
		texts = appendTexts(appendTexts(texts, n.List), n.ElseList)
	case *parse.WithNode:
		texts = appendTexts(appendTexts(texts, n.List), n.ElseList)
	}
	return texts
}
