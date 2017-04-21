package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var (
	linePat = regexp.MustCompile(`\w+\.go:\d+: +(\d+)\((\d+):(\d+)\) +# +(\d+) +(.+) +@0x([0-9a-z]+)(?: +(.+))?`)

	actPat = regexp.MustCompile(`(` +
		`^\+\+\+ +(\d+)\((\d+):(\d+)\) +copy` +
		`)|(` +
		`^=== +End` +
		`)|(` +
		`^=== +Handle` +
		`)`)

	kvPat = regexp.MustCompile(`^(\w+)=(.+)`)
)

type machID [3]int

var zeroMachID machID

type record struct {
	mid   machID
	count int
	ip    uint64
	act   string
	rest  string
}

type recordKind int

const (
	unknownLine = recordKind(iota)
	genericLine
	copyLine
	endLine
	hndlLine
)

func (ss sessions) parseRecord(line []byte) (rec record, kind recordKind) {
	match := linePat.FindSubmatch(line)
	if match == nil {
		kind = unknownLine
		return
	}

	kind = genericLine
	rec.mid[0], _ = strconv.Atoi(string(match[1]))
	rec.mid[1], _ = strconv.Atoi(string(match[2]))
	rec.mid[2], _ = strconv.Atoi(string(match[3]))
	rec.count, _ = strconv.Atoi(string(match[4]))
	rec.act = strings.TrimRight(string(match[5]), " \r\n")
	rec.ip, _ = strconv.ParseUint(string(match[6]), 16, 64)
	rec.rest = string(match[7])

	sess := ss.session(rec.mid)
	sess.mid = rec.mid
	sess.recs = append(sess.recs, rec)

	switch amatch := actPat.FindStringSubmatch(rec.act); {
	case amatch == nil:
	case amatch[1] != "": // copy
		kind = copyLine
		sess.pid[0], _ = strconv.Atoi(amatch[2])
		sess.pid[1], _ = strconv.Atoi(amatch[3])
		sess.pid[2], _ = strconv.Atoi(amatch[4])

	case amatch[5] != "": // end
		kind = endLine
		if match := kvPat.FindStringSubmatch(rec.rest); match != nil {
			sess := ss.session(rec.mid)
			switch string(match[1]) {
			case "err":
				sess.err = string(match[2])
			case "values":
				sess.values = string(match[2])
			default:
				log.Printf("UNKNOWN End key/val: %q = %q\n", match[1], match[2])
			}
		}

	case amatch[6] != "": // handle
		kind = hndlLine
	}

	return
}

type sessions map[machID]*session

type session struct {
	mid, pid machID
	recs     []record
	err      string
	values   string
	extra    []string
}

func (mid machID) String() string {
	return fmt.Sprintf("%d(%d:%d)", mid[0], mid[1], mid[2])
}

func (rec record) String() string {
	return fmt.Sprintf(
		"% 10v #% 4d @%#04x % -30s %q",
		rec.mid,
		rec.count,
		rec.ip,
		rec.act,
		rec.rest,
	)
}

func (ss sessions) session(mid machID) *session {
	sess := ss[mid]
	if sess == nil {
		sess = &session{mid: mid}
		ss[mid] = sess
	}
	return sess
}

func (ss sessions) extend(mid machID, s string) {
	sess := ss.session(mid)
	sess.extra = append(sess.extra, s)
}

func (ss sessions) walk(mid machID, f func(*session)) *session {
	sess := ss[mid]
	if sess == nil {
		return nil
	}

	var q []*session
	for sess.pid != zeroMachID {
		q = append(q, sess)
		sess = ss[sess.pid]
	}

	for {
		f(sess)
		if i := len(q) - 1; i >= 0 {
			sess, q = q[i], q[:i]
		} else {
			break
		}
	}
	return sess
}

func (ss sessions) sessionLog(sess *session, logf func(string, ...interface{})) {
	ss.walk(sess.mid, func(sess *session) {
		for _, rec := range sess.recs {
			logf("%v", rec)
		}
	})
	for _, line := range sess.extra {
		logf("%s", line)
	}
}

func parseSessions(r io.Reader) (sessions, error) {
	var tail machID
	sessions := make(sessions)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Bytes()
		switch rec, kind := sessions.parseRecord(line); kind {
		case unknownLine:
			if tail != zeroMachID {
				extra := strings.TrimRight(string(line), " \r\n")
				sessions.extend(tail, extra)
			}
		case endLine:
			tail = rec.mid
		case hndlLine:
			tail = zeroMachID
		}
	}
	return sessions, sc.Err()
}

func main() {
	sessions, err := parseSessions(os.Stdin)
	if err != nil {
		log.Fatal(err)
	}

	for _, sess := range sessions {
		if sess.err != "" {
			continue
		}
		fmt.Printf("%v %v\n", sess.mid, sess.values)
		sessions.sessionLog(sess, func(format string, args ...interface{}) {
			fmt.Printf("	"+format+"\n", args...)
		})
		fmt.Println()
	}
}