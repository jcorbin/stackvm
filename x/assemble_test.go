package xstackvm_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	. "github.com/jcorbin/stackvm/x"
)

type assemblerCases []assemblerCase

func (cs assemblerCases) run(t *testing.T) {
	for _, c := range cs {
		t.Run(c.name, c.run)
	}
}

type assemblerCase struct {
	name string
	in   []interface{}
	out  []byte
	err  string
}

func (c assemblerCase) run(t *testing.T) {
	assemblerTest{c, t}.run()
}

type assemblerTest struct {
	assemblerCase
	*testing.T
}

func (t assemblerTest) run() {
	prog, err := Assemble(t.in...)
	if t.err == "" {
		require.NoError(t, err, "unexpected error")
	} else {
		assert.EqualError(t, err, t.err, "expected error")
	}
	assert.Equal(t, t.out, prog, "expected machine code")
}

func TestAssemble(t *testing.T) {
	assemblerCases{
		{
			name: "bad token",
			in:   []interface{}{'X'},
			err:  `invalid token int32(88); expected "label:", ":ref", "opName", or an int`,
		},

		{
			name: "broken op",
			in:   []interface{}{99, 44},
			err:  `invalid token int(44); expected "opName"`,
		},

		{
			name: "invalid op",
			in:   []interface{}{42, "nope"},
			err:  `no such operation "nope"`,
		},

		{
			name: "invalid rep op",
			in:   []interface{}{":such", "nope"},
			err:  `no such operation "nope"`,
		},

		{
			name: "undefined ref",
			in:   []interface{}{":such", "jump"},
			err:  `undefined label "such"`,
		},

		{
			name: "basic",
			in: []interface{}{
				2, "push",
				3, "add",
				5, "eq",
			},
			out: []byte{
				0x82, 0x00,
				0x83, 0x11,
				0x85, 0x1a,
			},
		},

		{
			name: "small loop",
			in: []interface{}{
				10, "push",
				"loop:",
				1, "sub",
				0, "gt",
				":loop", "jnz",
				"halt",
			},
			out: []byte{
				0x8a, 0x00,
				0x81, 0x12,
				0x80, 0x1c,
				0x8f, 0xff, 0xff, 0xff, 0xf6, 0x29,
				0x7f,
			},
		},

		{
			name: "fizzbuzz",
			in: []interface{}{
				3, "mod", ":fizz", "jz",
				5, "mod", ":buzz", "jz",
				":cont", "jump",

				"fizz:",
				5, "mod", ":fizzbuzz", "jz",
				3, "push",
				":cont", "jump",

				"buzz:",
				3, "mod", ":fizzbuzz", "jz",
				5, "push",
				":cont", "jump",

				"fizzbuzz:",
				3, "push",
				5, "push",

				"cont:",
				"halt",
			},
			out: []byte{

				0x83, 0x15, 0x86, 0x2a, // f
				0x85, 0x15, 0x8a, 0x2a, // b
				0x94, 0x28, // c

				0x85, 0x15, 0x8c, 0x2a, // fb
				0x83, 0x00,
				0x8c, 0x28, // c

				0x83, 0x15, 0x84, 0x2a, // fb
				0x85, 0x00,
				0x84, 0x28, // c

				0x83, 0x00,
				0x85, 0x00,

				0x7f,
			},
		},
	}.run(t)
}
