package stackvm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

var (
	errNoArg           = errors.New("operation does not accept an argument")
	errVarOpts         = errors.New("truncated options")
	errTruncatedString = errors.New("truncated string")
	errTruncatedVarint = errors.New("truncated varint")
	errBigVarint       = errors.New("varint too big")
)

// NoSuchOpError is returned by ResolveOp if the named operation is not //
// defined.
type NoSuchOpError string

func (name NoSuchOpError) Error() string {
	return fmt.Sprintf("no such operation %q", string(name))
}

// New creates a new stack machine with a given program loaded. It takes a
// varcoded (more on that below) program, and an optional handler.
//
// When a non-nil handler is given, a queue is setup to handle copies of the
// machine at runtime. This handler will be called with each one after it has
// halted (explicitly, crashed, or due to an error). Without a queue, machine
// copy operations will fail (such as fork and branch).
//
// The "varcode" encoding scheme used is a variation on a varint:
// - the final byte of the varint (the one without the high bit set) encodes a
//   7-bit code-word
// - prior bytes (with their high bits set) encode an associated uint32 value
//
// This scheme is used first to encode options used to setup the machine, and
// then to encode the program that the machine will run.
//
// Valid option codes:
// - 0x00 end: indicates the end of options (beginning of program); must not
//   have a parameter.
// - 0x01 stack size: its required parameter declares the amount of memory
//   given to the parameter and control stacks (see below for details). The
//   size must be a multiple of 4 (32-bit word size). Default: 0x40.
// - 0x02 queue size: its required parameter specifies a maximum limit on how
//   many machine copies may be queued. Once this limit is reached operations
//   like fork and branch fail with queue-full error. Default: 10.
// - 0x03 max ops: its optional parameter declares a limit on the number of
//   program operations that can be executed by a single machine (the runtime
//   operation count is not shared between machine copies).
// - 0x04 max copies: its optional parameter declares a limit on the number
//   of machine copies that may be made in total. Well behaved programs
//   shouldn't need to specify this option, it should be mostly used for
//   debugging. Default: 0.
// - 0x05 entry: its required parameter is the value for IP instead of
//   starting execution at the top of the loaded program (right after the
//   stack).
// - 0x06 input: its required parameter is an endpoint of an input region;
//   must appear in start/end pairs.
// - 0x07 output: its required parameter is an endpoint of an output region;
//   must appear in start/end pairs.
// - 0x08 name: its required parameter is the address of a string name for
//   the last output or input region.
// - 0x09 addr labels: its required parameter is the count of how many
//   addr/label pairs follow this option. Each addr is encoded as a varint,
//   and each label is encoded with a varint length prefix followed by that
//   many bytes of utf-8 text.
// - 0x0a span open: its required parameter marks an address as a semantic
//   span open. A semantic span marks something like a function call.
// - 0x0b span close: its required parameter marks an address as a semantic
//   span close.
// - 0x7f version: reserved for future use, where its parameter will be the
//   required machine/program version; passing a version value is currently
//   unsupported.
//
// The stack space, declared by above option or 0x40 default, is shared by the
// Parameter Stack (PS) and Control Stack (CS) which grow towards each other:
// - PS grows up from PBP=0 (PS Base Pointer) to at most stacksize bytes
// - CS grows down from CBP=stacksize-1 (CS Base Pointer) towards PS
// - the head of PS is stored in PSP (Parameter Stack Pointer)
// - the head of CS is stored in CSP (Control Stack Pointer)
// - if PSP and CSP would touch a stack overflow error occurs (reported against
//   which ever stack tried to push a value)
// - similarly an undeflow will occur if PSP would go under PBP (negative)
// - likewise an undeflow happens if CSP would go over CBP
//
// The rest of prog is loaded in memory immediately after the stack space.
// Except for data sections, prog contains varcoded operations. Each operation
// has a 7-bit opcode, and an optional 32-bit immediate value. Most operations
// treat their immediate as an optional alternative to popping a value from the
// parameter stack.
//
// The Instruction Pointer (IP) is initialized to point at the first byte after
// the stack space (0x40 by default). Machine execution then happens (under
// .Run or .Step) by decoding a varcoded operation at IP, and executing it. If
// IP becomes corrupted (points to arbitrary memory), the machine will most
// likely crash explicitly (since memory defaults to 0-filled, and the 0 opcode
// is "crash") or halt with a decode error.
//
// TODO: document operations.
func New(prog []byte, mbos ...MachBuildOpt) (*Mach, error) {
	var mb machBuilder
	if err := mb.build(prog, mbos...); err != nil {
		return nil, err
	}
	return &mb.Mach, nil
}

// MachBuildOpt is an opaque option to build a New() machine.
type MachBuildOpt func(*machBuilder) error

// Handler passes a MachHandler to New()ly built machine.
func Handler(h MachHandler) MachBuildOpt {
	return func(mb *machBuilder) error {
		const pagesPerMachineGuess = 4
		n := int(mb.queueSize)
		mb.Mach.ctx.MachHandler = h
		mb.Mach.ctx.queue = newRunq(n)
		mb.Mach.ctx.machAllocator = makeMachFreeList(n)
		mb.Mach.ctx.pageAllocator = makePageFreeList(n * pagesPerMachineGuess)
		if mb.maxCopies > 0 {
			mb.Mach.ctx.machAllocator = maxMachCopiesAllocator(mb.maxCopies, mb.Mach.ctx.machAllocator)
		}
		return nil
	}
}

// Input passes a collection of input values to a New()ly built machine. For
// each Input(...) the loaded program must have defined an input region.
// Furthermore the number of values must fit with the corresponding input
// region, but do not have to fill it.
func Input(vals []uint32) MachBuildOpt {
	return func(mb *machBuilder) error {
		if mb.nextIn >= len(mb.inputs) {
			return fmt.Errorf("unsupported input[%d], only %d are defined", mb.nextIn+1, len(mb.inputs))
		}
		rg := mb.inputs[mb.nextIn]
		mb.nextIn++
		if n := rg.to - rg.from; len(vals) > int(n) {
			return fmt.Errorf("too many values for input[%d], max is %d, got %d", mb.nextIn, n, len(vals))
		}
		buf := make([]byte, len(vals)*4)
		for i, val := range vals {
			ByteOrder.PutUint32(buf[4*i:], val)
		}
		mb.Mach.storeBytes(rg.from, buf)
		return nil
	}
}

// NamedInput passes a named collection of input values to a New()ly built
// machine. For each NamedInput(...) the loaded program must have defined an
// input region with that name. Furthermore the number of values must fit with
// the corresponding input region, but do not have to fill it.
func NamedInput(name string, vals []uint32) MachBuildOpt {
	return func(mb *machBuilder) error {
		for _, rg := range mb.inputs {
			if rg.name == 0 {
				continue
			}
			rgName, err := mb.Mach.fetchString(rg.name)
			if err != nil {
				return err
			}
			if rgName == name {
				if n := rg.to - rg.from; len(vals) > int(n) {
					return fmt.Errorf("too many values for input[name=%q], max is %d, got %d", rgName, n, len(vals))
				}
				buf := make([]byte, len(vals)*4)
				for i, val := range vals {
					ByteOrder.PutUint32(buf[4*i:], val)
				}
				mb.Mach.storeBytes(rg.from, buf)
				return nil
			}
		}
		return fmt.Errorf("no named input region %q defined", name)
	}
}

// DebugInfo provides debug annotations about addresses in a machine's memory.
type DebugInfo interface {
	// Labels returns any labels defined for the given address.
	Labels(addr uint32) []string

	// Span returns whether the given address indicates a semantic span open or
	// close (both are possible!). A semantic span represents some structure
	// like a function call.
	Span(addr uint32) (open, close bool)

	// LabeledAddrs returns a slice of all addresses that have defined labels.
	LabeledAddrs() []uint32

	// SpanAddrs returns a slice of all addresses that have span mark(s).
	SpanAddrs() []uint32
}

// WithDebugInfo calls the given function with any defined debug info; if no
// debug info was defined, the function isn't called.
func WithDebugInfo(cb func(DebugInfo)) MachBuildOpt {
	return func(mb *machBuilder) error {
		if !mb.dbg.empty() {
			cb(mb.dbg)
		}
		return nil
	}
}

func (m *Mach) String() string {
	var buf bytes.Buffer
	buf.WriteString("Mach")
	if m.err != nil {
		if code, halted := m.halted(); halted {
			// TODO: symbolicate
			fmt.Fprintf(&buf, " HALT:%v", code)
		} else {
			fmt.Fprintf(&buf, " ERR:%v", m.err)
		}
	}
	fmt.Fprintf(&buf, " @0x%04x 0x%04x:0x%04x 0x%04x:0x%04x", m.ip, m.pbp, m.psp, m.cbp, m.csp)
	// TODO:
	// pages?
	// stack dump?
	// context describe?
	return buf.String()
}

// EachPage calls a function with each allocated section of memory; it MUST NOT
// mutate the memory, and should copy out any data that it needs to retain.
func (m *Mach) EachPage(f func(addr uint32, p *[_pageSize]byte) error) error {
	for i, pg := range m.pages {
		if pg != nil {
			if err := f(uint32(i*_pageSize), &pg.d); err != nil {
				return err
			}
		}
	}
	return nil
}

// Fetch fetches a single word from memory, returning it or an
// error.
func (m *Mach) Fetch(addr uint32) (uint32, error) { return m.fetch(addr) }

var zeroPageData [_pageSize]byte

// WriteTo writes all machine memory to the given io.Writer, returning the
// number of bytes written.
func (m *Mach) WriteTo(w io.Writer) (n int64, err error) {
	for _, pg := range m.pages {
		var wn int
		if pg == nil {
			wn, err = w.Write(zeroPageData[:])
		} else {
			wn, err = w.Write(pg.d[:])
		}
		n += int64(wn)
		if err != nil {
			break
		}
	}
	return
}

// IP returns the current instruction pointer.
func (m *Mach) IP() uint32 { return m.ip }

// PBP returns the current parameter stack base pointer.
func (m *Mach) PBP() uint32 { return m.pbp }

// PSP returns the current parameter stack pointer.
func (m *Mach) PSP() uint32 {
	if m.psp > m.cbp {
		return m.pbp
	}
	return m.psp
}

// CBP returns the current control stack base pointer.
func (m *Mach) CBP() uint32 { return m.cbp }

// CSP returns the current control stack pointer.
func (m *Mach) CSP() uint32 { return m.csp }

// Values returns any output values from the machine. Output values may be
// statically declared via the output option. Additionally, once the machine
// has halted with 0 status code, 0 or more pairs of output ranges may be left
// on the control stack.
func (m *Mach) Values() ([][]uint32, error) {
	_, vals, err := m.outValues()
	return vals, err
}

// NamedValues returns any output values from the machine, along with any
// declared names. Statically decalde output values carry the name of their
// label. There's currently no support for dynamically declared outputs to be
// named
func (m *Mach) NamedValues() (map[string][]uint32, error) {
	outputs, vals, err := m.outValues()
	if err != nil {
		return nil, err
	}
	if len(vals) == 0 {
		return nil, nil
	}
	nvs := make(map[string][]uint32, len(vals))
	for i, v := range vals {
		if rg := outputs[i]; rg.name != 0 {
			name, err := m.fetchString(rg.name)
			if err != nil {
				return nil, err
			}
			nvs[name] = v
			continue
		}
		nvs[fmt.Sprintf("unnamed_output_%d", i)] = v
	}
	return nvs, nil
}

// Region describes an output region returned by Mach.Outputs.
type Region struct {
	Name     string
	From, To uint32
}

// Outputs returns a slice of the machine's (currently) defined output regions,
// as would be used by Values or NamedValues.
func (m *Mach) Outputs() ([]Region, error) {
	outputs, err := m.outputs()
	if len(outputs) == 0 || err != nil {
		return nil, err
	}
	rgs := make([]Region, len(outputs))
	for i, rg := range outputs {
		rgs[i] = Region{From: rg.from, To: rg.to}
		if rg.name != 0 {
			name, err := m.fetchString(rg.name)
			if err != nil {
				return nil, err
			}
			rgs[i].Name = name
		}
	}
	return rgs, nil
}

func (m *Mach) outputs() ([]region, error) {
	done := false
	if m.err != nil {
		if arg, ok := m.halted(); !ok || arg != 0 {
			return nil, m.err
		}
		done = true
	}
	outputs := m.ctx.outputs
	outputs = outputs[:len(outputs):len(outputs)]
	if done {
		cs, err := m.fetchCS()
		if err != nil {
			return nil, err
		}
		if len(cs) > 0 {
			if len(cs)%2 != 0 {
				return nil, fmt.Errorf("invalid control stack length %d", len(cs))
			}
			outputs = append(make([]region, 0, len(outputs)+len(cs)/2), outputs...)
			for i := 0; i < len(cs); i += 2 {
				outputs = append(outputs, region{from: cs[i], to: cs[i+1]})
			}
		}
	}
	return outputs, nil
}

func (m *Mach) outValues() ([]region, [][]uint32, error) {
	outputs, err := m.outputs()
	if err != nil {
		return nil, nil, err
	}
	if len(outputs) == 0 {
		return nil, nil, nil
	}
	res := make([][]uint32, 0, len(outputs))
	for _, rg := range outputs {
		ns, err := m.fetchMany(rg.from, rg.to)
		if err != nil {
			return nil, nil, err
		}
		res = append(res, ns)
	}
	return outputs, res, nil
}

// Stacks returns the current values on the parameter and control
// stacks.
func (m *Mach) Stacks() ([]uint32, []uint32, error) {
	ps, err := m.fetchPS()
	if err != nil {
		return nil, nil, err
	}
	cs, err := m.fetchCS()
	if err != nil {
		return nil, nil, err
	}
	return ps, cs, nil
}

// MemCopy copies bytes from memory into the given buffer, returning
// the number of bytes copied.
func (m *Mach) MemCopy(addr uint32, bs []byte) int {
	return m.fetchBytes(addr, bs)
}

// Tracer is the interface taken by (*Mach).Trace to observe machine
// execution: Begin() and End() are called when a machine starts and finishes
// respectively; Before() and After() are around each machine operation;
// Queue() is called when a machine creates a copy of itself; Handle() is
// called after an ended machine has been passed to any result handling
// function.
//
// Contextual information may be made available by implementing the Context()
// method: if a tracer wants defines a value for some key, it should return
// that value and a true boolean. Tracers, and other code, may then use
// (*Mach).Tracer().Context() to access contextual information from other
// tracers.
type Tracer interface {
	Context(m *Mach, key string) (interface{}, bool)
	Begin(m *Mach)
	Before(m *Mach, ip uint32, op Op)
	After(m *Mach, ip uint32, op Op)
	Queue(m, n *Mach)
	End(m *Mach)
	Handle(m *Mach, err error)
}

// Op is used within Tracer to pass along decoded machine operations.
type Op struct {
	Code byte
	Arg  uint32
	Have bool
}

// ResolveOp builds an op given a name string, and argument.
func ResolveOp(name string, arg uint32, have bool) (Op, error) {
	code, def := opName2Code[name]
	if !def {
		return Op{}, NoSuchOpError(name)
	}
	if have && ops[code].imm.kind() == opImmNone {
		return Op{}, errNoArg
	}
	return Op{code, arg, have}, nil
}

// Name returns the name of the coded operation.
func (o Op) Name() string {
	return ops[o.Code].name
}

// Generates part of the New() documentation from the inline docs below.
//go:generate python collect_docs.py -i api.go -o api.go optCode "^// Valid option codes:" "^//$"

const (
	// indicates the end of options (beginning of program); must not have a
	// parameter.
	optCodeEnd uint8 = 0x00

	// its required parameter declares the amount of memory given to the
	// parameter and control stacks (see below for details). The size must be a
	// multiple of 4 (32-bit word size). Default: 0x40.
	optCodeStackSize = 0x01

	// its required parameter specifies a maximum limit on how many machine
	// copies may be queued. Once this limit is reached operations like fork
	// and branch fail with queue-full error. Default: 10.
	optCodeQueueSize = 0x02

	// its optional parameter declares a limit on the number of program
	// operations that can be executed by a single machine (the runtime
	// operation count is not shared between machine copies).
	optCodeMaxOps = 0x03

	// its optional parameter declares a limit on the number of machine copies
	// that may be made in total. Well behaved programs shouldn't need to
	// specify this option, it should be mostly used for debugging. Default: 0.
	optCodeMaxCopies = 0x04

	// its required parameter is the value for IP instead of starting execution
	// at the top of the loaded program (right after the stack).
	optCodeEntry = 0x05

	// its required parameter is an endpoint of an input region; must appear in
	// start/end pairs.
	optCodeInput = 0x06

	// its required parameter is an endpoint of an output region; must appear
	// in start/end pairs.
	optCodeOutput = 0x07

	// its required parameter is the address of a string name for the last
	// output or input region.
	optCodeName = 0x08

	// its required parameter is the count of how many addr/label pairs follow
	// this option. Each addr is encoded as a varint, and each label is encoded
	// with a varint length prefix followed by that many bytes of utf-8 text.
	optCodeAddrLabels = 0x09

	// its required parameter marks an address as a semantic span open. A
	// semantic span marks something like a function call.
	optCodeSpanOpen = 0x0a

	// its required parameter marks an address as a semantic span close.
	optCodeSpanClose = 0x0b

	// reserved for future use, where its parameter will be the required
	// machine/program version; passing a version value is currently
	// unsupported.
	optCodeVersion = 0x7f
)

type anno uint8

const (
	annoSpanOpen anno = 1 << iota
	annoSpanClose
)

type debugInfo struct {
	labels map[uint32][]string
	annos  map[uint32]anno
}

func (dbg debugInfo) Labels(addr uint32) []string {
	return dbg.labels[addr]
}

func (dbg debugInfo) Span(addr uint32) (open, close bool) {
	an := dbg.annos[addr]
	open = an&annoSpanOpen != 0
	close = an&annoSpanClose != 0
	return
}

func (dbg debugInfo) empty() bool {
	return len(dbg.labels) == 0 && len(dbg.annos) == 0
}

func (dbg debugInfo) LabeledAddrs() []uint32 {
	addrs := make([]uint32, 0, len(dbg.labels))
	for addr := range dbg.labels {
		addrs = append(addrs, addr)
	}
	return addrs
}

func (dbg debugInfo) SpanAddrs() []uint32 {
	addrs := make([]uint32, 0, len(dbg.annos))
	for addr := range dbg.annos {
		addrs = append(addrs, addr)
	}
	return addrs
}

func (dbg *debugInfo) addLabel(addr uint32, label string) {
	if dbg.labels == nil {
		dbg.labels = make(map[uint32][]string)
	}
	dbg.labels[addr] = append(dbg.labels[addr], label)
}

func (dbg *debugInfo) annotate(addr uint32, an anno) {
	if dbg.annos == nil {
		dbg.annos = make(map[uint32]anno)
	}
	dbg.annos[addr] |= an
}

type machBuilder struct {
	Mach
	base      uint32
	queueSize int
	maxCopies int
	inputs    []region
	nextIn    int
	dbg       debugInfo

	buf []byte
	h   MachHandler
	n   int
}

func (mb *machBuilder) build(buf []byte, mbos ...MachBuildOpt) error {
	mb.queueSize = defaultQueueSize

	mb.Mach.ctx.MachHandler = defaultHandler
	mb.Mach.ctx.queue = noQueue
	mb.Mach.ctx.machAllocator = defaultMachAllocator
	mb.Mach.ctx.pageAllocator = defaultPageAllocator
	mb.Mach.psp = _pspInit

	mb.buf = buf

	if err := mb.handleOpts(); err != nil {
		return err
	}

	prog := mb.buf[mb.n:]
	mb.Mach.opc = makeOpCache(len(prog))
	mb.Mach.storeBytes(mb.base, prog)
	// TODO mark code segment, update data

	for _, mbo := range mbos {
		if err := mbo(mb); err != nil {
			return err
		}
	}

	return nil
}

func (mb *machBuilder) handleOpts() error {
	for {
		code, arg, err := mb.readOptCode()
		if err != nil {
			return err
		}
		if done, err := mb.handleOpt(code, arg); err != nil {
			return err
		} else if done {
			return nil
		}
	}
}

func (mb *machBuilder) readOptCode() (uint8, uint32, error) {
	n, arg, code, ok := readVarCode(mb.buf[mb.n:])
	mb.n += n
	if !ok {
		return 0, 0, errVarOpts
	}
	return code, arg, nil
}

func (mb *machBuilder) mayReadOptCode(ifCode uint8) (uint32, bool, error) {
	n, arg, code, ok := readVarCode(mb.buf[mb.n:])
	mb.n += n
	if !ok {
		return 0, false, errVarOpts
	}
	if code != ifCode {
		mb.n -= n
		return 0, false, nil
	}
	return arg, true, nil
}

func (mb *machBuilder) readAddrLabels(n int) error {
	for i := 0; i < n; i++ {
		addr, err := mb.readUvarint()
		if err != nil {
			return fmt.Errorf("bad address: %v", err)
		}
		label, err := mb.readString()
		if err != nil {
			return fmt.Errorf("bad label: %v", err)
		}
		mb.dbg.addLabel(addr, label)
	}
	return nil
}

func (mb *machBuilder) readString() (string, error) {
	v, err := mb.readUvarint()
	if err != nil {
		return "", fmt.Errorf("bad string length: %v", err)
	}
	n := int(v)
	n += mb.n
	if n >= len(mb.buf) {
		return "", errTruncatedString
	}
	s := string(mb.buf[mb.n:n])
	mb.n = n
	return s, nil
}

func (mb *machBuilder) readUvarint() (uint32, error) {
	v, n := binary.Uvarint(mb.buf[mb.n:])
	v32 := uint32(v)
	if n == 0 {
		return 0, errTruncatedVarint
	} else if n < 0 || uint64(v32) != v {
		return 0, errBigVarint
	}
	mb.n += n
	return v32, nil
}

func (mb *machBuilder) handleOpt(code uint8, arg uint32) (bool, error) {
	switch code {

	case optCodeVersion:
	case 0x80 | optCodeVersion:
		if arg != 0 {
			return false, fmt.Errorf("unsupported machine version %v", arg)
		}

	case 0x80 | optCodeStackSize:
		if arg > 0xffff {
			return false, fmt.Errorf("invalid stacksize %#x", arg)
		}
		if arg%4 != 0 {
			return false, fmt.Errorf("invalid stacksize %#02x, not a word-multiple", arg)
		}
		oldBase := mb.Mach.cbp + 4
		mb.base = uint32(arg)
		if mb.base > 0 {
			mb.Mach.cbp = mb.base - 4
			mb.Mach.csp = mb.base - 4
		}
		// TODO: else support 0
		if mb.Mach.ip == 0 || mb.Mach.ip == oldBase {
			mb.Mach.ip = mb.base
		}

	case 0x80 | optCodeQueueSize:
		mb.queueSize = int(arg)

	case optCodeMaxOps:
		mb.Mach.limit = 0

	case 0x80 | optCodeMaxOps:
		mb.Mach.limit = uint(arg)

	case optCodeMaxCopies:
		mb.maxCopies = 0

	case 0x80 | optCodeMaxCopies:
		mb.maxCopies = int(arg)

	case 0x80 | optCodeEntry:
		mb.Mach.ip = arg

	case 0x80 | optCodeInput:
		start := arg
		code, end, err := mb.readOptCode()
		if err != nil {
			return false, err
		}
		if code != 0x80|optCodeInput {
			return false, fmt.Errorf("unpaired input opt code, got %#02x instead", code)
		}
		rg := region{from: start, to: end}
		if addr, named, err := mb.mayReadOptCode(0x80 | optCodeName); err != nil {
			return false, err
		} else if named {
			rg.name = addr
		}
		mb.inputs = append(mb.inputs, rg)

	case 0x80 | optCodeOutput:
		start := arg
		code, end, err := mb.readOptCode()
		if err != nil {
			return false, err
		}
		if code != 0x80|optCodeOutput {
			return false, fmt.Errorf("unpaired output opt code, got %#02x instead", code)
		}
		rg := region{from: start, to: end}
		if addr, named, err := mb.mayReadOptCode(0x80 | optCodeName); err != nil {
			return false, err
		} else if named {
			rg.name = addr
		}
		mb.Mach.ctx.outputs = append(mb.Mach.ctx.outputs, rg)

	case 0x80 | optCodeAddrLabels:
		if err := mb.readAddrLabels(int(arg)); err != nil {
			return false, err
		}

	case 0x80 | optCodeSpanOpen:
		mb.dbg.annotate(arg, annoSpanOpen)

	case 0x80 | optCodeSpanClose:
		mb.dbg.annotate(arg, annoSpanClose)

	case optCodeEnd:
		return true, nil

	default:
		return false, fmt.Errorf("invalid option code=%#02x have=%v arg=%#x", code&0x7f, code&0x80 != 0, arg)
	}

	return false, nil
}

// TODO: refactor Op and co around a reifiied varcode dialect type.

// ResolveOptionRefArg is like Op.ResolveRefArg, except for the option varcode
// dialect.
func ResolveOptionRefArg(op Op, site, targ uint32) Op {
	if optionAcceptsRef(op) {
		op.Arg = targ
	} else {
		panic(fmt.Sprintf("%v opt does not accept ref args", NameOption(op.Code)))
	}
	return op
}

// optionAcceptsRef is like Op.AcceptsRef, except for the option varcode
// dialect.
func optionAcceptsRef(op Op) bool {
	switch op.Code {
	case optCodeEntry, optCodeInput, optCodeOutput, optCodeName, optCodeSpanOpen, optCodeSpanClose:
		return true
	}
	return false
}

// OptionNeededSize is like Op.NeededSize, except for the option varcode
// dialect.
func OptionNeededSize(op Op) int {
	if optionAcceptsRef(op) {
		return MaxVarCodeLen
	}
	if op.Code == optCodeAddrLabels {
		return MaxVarCodeLen
	}
	return op.NeededSize()
}

// NameOption retruns the name string for an option code.
func NameOption(code uint8) string {
	switch code & 0x7f {
	case optCodeEnd:
		return "end"
	case optCodeStackSize:
		return "stackSize"
	case optCodeQueueSize:
		return "queueSize"
	case optCodeMaxOps:
		return "maxOps"
	case optCodeMaxCopies:
		return "maxCopies"
	case optCodeEntry:
		return "entry"
	case optCodeInput:
		return "input"
	case optCodeOutput:
		return "output"
	case optCodeName:
		return "name"
	case optCodeAddrLabels:
		return "addrLabels"
	case optCodeSpanOpen:
		return "spanOpen"
	case optCodeSpanClose:
		return "spanClose"
	case optCodeVersion:
		return "version"
	default:
		return fmt.Sprintf("?<%02x>", code)
	}
}

// ResolveOption constructs an option Op.
func ResolveOption(name string, arg uint32, have bool) (op Op) {
	switch name {
	case "end":
		op.Code = optCodeEnd
	case "stackSize":
		op.Code = optCodeStackSize
	case "queueSize":
		op.Code = optCodeQueueSize
	case "maxOps":
		op.Code = optCodeMaxOps
	case "maxCopies":
		op.Code = optCodeMaxCopies
	case "entry":
		op.Code = optCodeEntry
	case "input":
		op.Code = optCodeInput
	case "output":
		op.Code = optCodeOutput
	case "name":
		op.Code = optCodeName
	case "addrLabels":
		op.Code = optCodeAddrLabels
	case "spanOpen":
		op.Code = optCodeSpanOpen
	case "spanClose":
		op.Code = optCodeSpanClose
	case "version":
		op.Code = optCodeVersion
	default:
		return
	}
	op.Arg = arg
	op.Have = have
	return
}

// EncodeInto encodes the operation into the given buffer, returning the number
// of bytes encoded.
func (o Op) EncodeInto(p []byte) int {
	c := uint8(o.Code)
	if o.Have {
		c |= 0x80
	}
	return putVarCode(p, o.Arg, c)
}

// NeededSize returns the number of bytes needed to encode op.
func (o Op) NeededSize() int {
	if o.AcceptsRef() {
		return MaxVarCodeLen
	}
	c := uint8(o.Code)
	if o.Have {
		c |= 0x80
	}
	return varCodeLength(o.Arg, c)
}

// AcceptsRef return true only if the argument can resolve another op reference
// ala ResolveRefArg.
func (o Op) AcceptsRef() bool {
	switch ops[o.Code].imm.kind() {
	case opImmVal, opImmOffset, opImmAddr:
		return true
	}
	return false
}

// ResolveRefArg fills in the argument of a control op relative to another op's
// encoded location, and the current op's.
func (o Op) ResolveRefArg(myIP, targIP uint32) Op {
	switch ops[o.Code].imm.kind() {
	case opImmOffset:
		// need to skip the arg and the code...
		c := uint8(o.Code)
		if o.Have {
			c |= 0x80
		}

		d := targIP - myIP
		n := varCodeLength(d, c)
		d -= uint32(n)
		if id := int32(d); id < 0 && varCodeLength(uint32(id), c) != n {
			// ...arg off by one, now that we know its value.
			id--
			d = uint32(id)
		}
		o.Arg = d

	case opImmVal, opImmAddr:
		o.Arg = targIP

	default:
		panic(fmt.Sprintf("%v op does not accept ref args", o.Name()))
	}
	return o
}

func (o Op) String() string {
	def := ops[o.Code]
	if !o.Have {
		return def.name
	}
	switch def.imm.kind() {
	case opImmVal:
		return fmt.Sprintf("%d %s", o.Arg, def.name)
	case opImmAddr:
		return fmt.Sprintf("@%#04x %s", o.Arg, def.name)
	case opImmOffset:
		return fmt.Sprintf("%+#05x %s", int32(o.Arg), def.name)
	}
	return fmt.Sprintf("INVALID(%#x %x %q)", o.Arg, o.Code, def.name)
}

// Tracer returns the current Tracer that the machine is running under, if any.
func (m *Mach) Tracer() Tracer {
	mt1, ok1 := m.ctx.MachHandler.(*machTracer)
	mt2, ok2 := m.ctx.queue.(*machTracer)
	if !ok1 && !ok2 {
		return nil
	}
	if !ok1 || !ok2 || mt1 != mt2 {
		panic("broken machTracer setup")
	}
	return mt1.t
}

type machTracer struct {
	MachHandler
	queue
	t Tracer
	m *Mach
}

func fixTracer(t Tracer, m *Mach) {
	h := m.ctx.MachHandler
	for mt, ok := h.(*machTracer); ok; mt, ok = h.(*machTracer) {
		h = mt.MachHandler
	}
	q := m.ctx.queue
	for mt, ok := q.(*machTracer); ok; mt, ok = q.(*machTracer) {
		q = mt.queue
	}
	mt := &machTracer{h, q, t, m}
	m.ctx.MachHandler = mt
	m.ctx.queue = mt
}

const defaultQueueSize = 10

func (mt *machTracer) Enqueue(n *Mach) error {
	mt.t.Queue(mt.m, n)
	fixTracer(mt.t, n)
	return mt.queue.Enqueue(n)
}

// Trace implements the same logic as (*Mach).run, but calls a Tracer
// at the appropriate times.
func (m *Mach) Trace(t Tracer) error {
	// the code below is essentially an
	// instrumented copy of Mach.Run (with mach.run
	// inlined)

	orig := m

	fixTracer(t, m)

repeat:
	// live
	t.Begin(m)
	for m.err == nil {
		var readOp Op
		if _, code, arg, err := m.read(m.ip); err != nil {
			m.err = err
			break
		} else {
			readOp = Op{code.code(), arg, code.hasImm()}
		}
		t.Before(m, m.ip, readOp)
		m.step()
		if m.err != nil {
			break
		}
		t.After(m, m.ip, readOp)
	}
	t.End(m)

	// win or die
	err := m.ctx.Handle(m)
	t.Handle(m, err)
	if err == nil {
		if n := m.ctx.Dequeue(); n != nil {
			m.free()
			m = n
			// die
			goto repeat
		}
	}

	// win?
	if m != orig {
		*orig = *m
	}
	return err
}

// Run runs the machine until termination, returning any error.
func (m *Mach) Run() error {
	n, err := m.run()
	if n != m {
		*m = *n
	}
	return err
}

// Step single steps the machine; it decodes and executes one
// operation.
func (m *Mach) Step() error {
	if m.err == nil {
		m.step()
	}
	return m.Err()
}

// HaltCode returns the halt code and true if the machine has halted
// normally; otherwise false is returned.
func (m *Mach) HaltCode() (uint32, bool) { return m.halted() }

var (
	lowHaltErrors [256]error
	haltErrors    = make(map[uint32]error)
)

func init() {
	for i := 0; i < len(lowHaltErrors); i++ {
		lowHaltErrors[i] = fmt.Errorf("HALT(%d)", i)
	}
}

// Err returns the last error from machine execution, wrapped with
// execution context.
func (m *Mach) Err() error {
	err := m.err
	if code, halted := m.halted(); halted {
		if code == 0 {
			return nil
		}
		if code < uint32(len(lowHaltErrors)) {
			err = lowHaltErrors[code]
		} else {
			he, def := haltErrors[code]
			if !def {
				he = fmt.Errorf("HALT(%d)", code)
				haltErrors[code] = he
			}
			err = he
		}
	}
	if err == nil {
		return nil
	}
	if _, ok := err.(MachError); !ok {
		return MachError{m.ip, err}
	}
	return err
}

// MachError wraps an underlying machine error with machine state.
type MachError struct {
	addr uint32
	err  error
}

// Cause returns the underlying machine error.
func (me MachError) Cause() error { return me.err }

func (me MachError) Error() string { return fmt.Sprintf("@0x%04x: %v", me.addr, me.err) }
