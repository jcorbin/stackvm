package xstackvm

import (
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/jcorbin/stackvm"
)

var errNotImplemented = errors.New("not implemented")

// Assemble builds a byte encoded machine program from a slice of
// operation names. Operations may be preceded by an immediate
// argument. An immediate argument may be an integer value, or a label
// reference string of the form ":name". Labels are defined with a string of
// the form "name:".
func Assemble(in ...interface{}) ([]byte, error) {
	toks, err := tokenize(in)
	if err != nil {
		return nil, err
	}

	ops, jumps, err := resolve(toks)
	if err != nil {
		return nil, err
	}

	if len(jumps) > 0 {
		return nil, errNotImplemented // TODO
	}
	sort.Ints(jumps)

	return assemble(ops, jumps), nil
}

// MustAssemble uses assemble the input, using Assemble(), and panics
// if it returns a non-nil error.
func MustAssemble(in ...interface{}) []byte {
	prog, err := Assemble(in...)
	if err != nil {
		panic(err)
	}
	return prog
}

type token struct {
	label, ref, op string
	imm            uint32
}

func label(s string) token  { return token{label: s} }
func ref(s string) token    { return token{ref: s} }
func opName(s string) token { return token{op: s} }
func imm(n int) token       { return token{imm: uint32(n)} }

func (t token) String() string {
	if t.op != "" {
		return t.op
	}
	if t.label != "" {
		return t.label + ":"
	}
	if t.ref != "" {
		return ":" + t.label
	}
	return strconv.Itoa(int(t.imm))
}

func tokenize(in []interface{}) (out []token, err error) {
	for i := 0; i < len(in); i++ {
		if s, ok := in[i].(string); ok {
			// label
			if j := len(s) - 1; j > 0 && s[j] == ':' {
				out = append(out, label(s[:j]))
				continue
			}

			// ref
			if len(s) > 1 && s[0] == ':' {
				out = append(out, ref(s[1:]))
				goto op
			}

			// opName
			out = append(out, opName(s))
			continue
		}

		// imm
		if n, ok := in[i].(int); ok {
			out = append(out, imm(n))
			goto op
		}

		return nil, fmt.Errorf(
			`invalid token %T(%v); expected "label:", ":ref", "opName", or an int`,
			in[i], in[i])

	op:
		i++
		// got r ref or v imm, must have opName
		if s, ok := in[i].(string); ok {
			out = append(out, opName(s))
			continue
		}

		return nil, fmt.Errorf(
			`invalid token %T(%v); expected "opName"`,
			in[i], in[i])
	}
	return
}

func resolve(toks []token) (ops []stackvm.Op, jumps []int, err error) {
	numJumps := 0
	labels := make(map[string]int)
	refs := make(map[string][]int)

	for i := 0; i < len(toks); i++ {
		tok := toks[i]

		if tok.label != "" {
			labels[tok.label] = len(ops)
			continue
		}

		if ref := tok.ref; ref != "" {
			i++
			tok = toks[i]
			op, err := stackvm.ResolveOp(tok.op, 0, true)
			if err != nil {
				return nil, nil, err
			}
			ops = append(ops, op)
			refs[ref] = append(refs[tok.ref], len(ops)-1)
			numJumps++
			continue
		}

		arg, have := uint32(0), false
		if tok.op == "" {
			arg, have = tok.imm, true
			i++
			tok = toks[i]
		}
		op, err := stackvm.ResolveOp(tok.op, arg, have)
		if err != nil {
			return nil, nil, err
		}
		ops = append(ops, op)
	}

	if numJumps > 0 {
		jumps = make([]int, 0, numJumps)
		for name, sites := range refs {
			i := labels[name]
			for _, j := range sites {
				ops[j].Arg = uint32(i - j - 1)
				jumps = append(jumps, j)
			}
		}
	}

	return
}

func assemble(ops []stackvm.Op, jumps []int) []byte {
	// allocate worst-case-estimated output space
	est := 0
	for i := range ops {
		if ops[i].Have {
			est += varOpLength(ops[i].Arg)
		}
		est++
	}

	return assembleInto(ops, make([]byte, est))
}

func assembleInto(ops []stackvm.Op, p []byte) []byte {
	offsets := make([]int, len(ops)+1)
	c, i := 0, 0 // current op offset and index
	for i < len(ops) {
		c += ops[i].EncodeInto(p[c:])
		i++
		offsets[i] = c
	}
	return p[:c]
}

func varOpLength(n uint32) (m int) {
	for v := n; v != 0; v >>= 7 {
		m++
	}
	m++
	return
}
