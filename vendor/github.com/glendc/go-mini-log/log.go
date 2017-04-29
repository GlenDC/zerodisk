// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package minilog

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"time"
)

// These flags define which text to prefix to each log entry generated by the Logger.
const (
	// Bits or'ed together to control what's printed.
	// There is no control over the order they appear (the order listed
	// here) or the format they present (as described in the comments).
	// The prefix is followed by a colon only when Llongfile or Lshortfile
	// is specified.
	// For example, flags Ldate | Ltime (or LstdFlags) produce,
	//	2009/01/23 01:23:23 message
	// while flags Ldate | Ltime | Lmicroseconds | Llongfile produce,
	//	2009/01/23 01:23:23.123123 /a/b/c/d.go:23: message
	Ldate         = 1 << iota     // the date in the local time zone: 2009/01/23
	Ltime                         // the time in the local time zone: 01:23:23
	Lmicroseconds                 // microsecond resolution: 01:23:23.123123.  assumes Ltime.
	Llongfile                     // full file name and line number: /a/b/c/d.go:23
	Lshortfile                    // final file name element and line number: d.go:23. overrides Llongfile
	LUTC                          // if Ldate or Ltime is set, use UTC rather than the local time zone
	LDebug                        // print debug statements
	LstdFlags     = Ldate | Ltime // initial values for the standard logger
)

const (
	tagDebug = "[DEBUG] "
	tagInfo  = "[INFO] "
	tagError = "[ERROR] "
)

// Logger defines a minimalistic Logger interface,
// restricting itself to the bare minimum.
type Logger interface {
	// verbose messages targeted at the developer
	Debug(args ...interface{})
	Debugf(format string, args ...interface{})
	// info messages targeted at the user and developer
	Info(args ...interface{})
	Infof(format string, args ...interface{})
	// a fatal message targeted at the user and developer
	// the program will exit as this message
	// this level shouldn't be used by libraries
	Fatal(args ...interface{})
	Fatalf(format string, args ...interface{})
}

// New creates a new Logger. The out variable sets the
// destination to which log data will be written.
// The prefix appears at the beginning of each generated log line.
// The flag argument defines the logging properties.
func New(out io.Writer, prefix string, flag int) Logger {
	return &miniLogger{out: out, prefix: prefix, flag: flag}
}

// A miniLogger represents an active logging object that generates lines of
// output to an io.Writer. Each logging operation makes a single call to
// the Writer's Write method. A Logger can be used simultaneously from
// multiple goroutines; it guarantees to serialize access to the Writer.
type miniLogger struct {
	mu     sync.Mutex // ensures atomic writes; protects the following fields
	prefix string     // prefix to write at beginning of each line
	flag   int        // properties
	out    io.Writer  // destination for output
	buf    []byte     // for accumulating text to write
}

// SetOutput sets the output destination for the logger.
func (l *miniLogger) SetOutput(w io.Writer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.out = w
}

var std = New(os.Stderr, "", LstdFlags).(*miniLogger)

// Cheap integer to fixed-width decimal ASCII.  Give a negative width to avoid zero-padding.
func itoa(buf *[]byte, i int, wid int) {
	// Assemble decimal in reverse order.
	var b [20]byte
	bp := len(b) - 1
	for i >= 10 || wid > 1 {
		wid--
		q := i / 10
		b[bp] = byte('0' + i - q*10)
		bp--
		i = q
	}
	// i < 10
	b[bp] = byte('0' + i)
	*buf = append(*buf, b[bp:]...)
}

func (l *miniLogger) formatHeader(buf *[]byte, t time.Time, file string, line int) {
	*buf = append(*buf, l.prefix...)
	if l.flag&LUTC != 0 {
		t = t.UTC()
	}
	if l.flag&(Ldate|Ltime|Lmicroseconds) != 0 {
		if l.flag&Ldate != 0 {
			year, month, day := t.Date()
			itoa(buf, year, 4)
			*buf = append(*buf, '/')
			itoa(buf, int(month), 2)
			*buf = append(*buf, '/')
			itoa(buf, day, 2)
			*buf = append(*buf, ' ')
		}
		if l.flag&(Ltime|Lmicroseconds) != 0 {
			hour, min, sec := t.Clock()
			itoa(buf, hour, 2)
			*buf = append(*buf, ':')
			itoa(buf, min, 2)
			*buf = append(*buf, ':')
			itoa(buf, sec, 2)
			if l.flag&Lmicroseconds != 0 {
				*buf = append(*buf, '.')
				itoa(buf, t.Nanosecond()/1e3, 6)
			}
			*buf = append(*buf, ' ')
		}
	}
	if l.flag&(Lshortfile|Llongfile) != 0 {
		if l.flag&Lshortfile != 0 {
			short := file
			for i := len(file) - 1; i > 0; i-- {
				if file[i] == '/' {
					short = file[i+1:]
					break
				}
			}
			file = short
		}
		*buf = append(*buf, file...)
		*buf = append(*buf, ':')
		itoa(buf, line, -1)
		*buf = append(*buf, ": "...)
	}
}

// Output writes the output for a logging event. The string s contains
// the text to print after the prefix specified by the flags of the
// Logger. A newline is appended if the last character of s is not
// already a newline. Calldepth is used to recover the PC and is
// provided for generality, although at the moment on all pre-defined
// paths it will be 2.
func (l *miniLogger) Output(calldepth int, s string) error {
	now := time.Now() // get this early.
	var file string
	var line int
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.flag&(Lshortfile|Llongfile) != 0 {
		// release lock while getting caller info - it's expensive.
		l.mu.Unlock()
		var ok bool
		_, file, line, ok = runtime.Caller(calldepth)
		if !ok {
			file = "???"
			line = 0
		}
		l.mu.Lock()
	}
	l.buf = l.buf[:0]
	l.formatHeader(&l.buf, now, file, line)
	l.buf = append(l.buf, s...)
	if len(s) == 0 || s[len(s)-1] != '\n' {
		l.buf = append(l.buf, '\n')
	}
	_, err := l.out.Write(l.buf)
	return err
}

// Debug implements Logger.Debug
func (l *miniLogger) Debug(args ...interface{}) {
	if l.Flags()&LDebug == 0 {
		return
	}

	l.Output(2, tagDebug+fmt.Sprintln(args...))
}

// Debugf implements Logger.Debugf
func (l *miniLogger) Debugf(format string, args ...interface{}) {
	if l.Flags()&LDebug == 0 {
		return
	}

	l.Output(2, tagInfo+fmt.Sprintf(format, args...))
}

// Info implements Logger.Info
func (l *miniLogger) Info(args ...interface{}) {
	l.Output(2, tagInfo+fmt.Sprintln(args...))
}

// Infof implements Logger.Infof
func (l *miniLogger) Infof(format string, args ...interface{}) {
	l.Output(2, tagInfo+fmt.Sprintf(format, args...))
}

// Fatal implements Logger.Fatal
func (l *miniLogger) Fatal(args ...interface{}) {
	l.Output(2, tagError+fmt.Sprintln(args...))
	os.Exit(1)
}

// Fatalf implements Logger.Fatalf
func (l *miniLogger) Fatalf(format string, args ...interface{}) {
	l.Output(2, tagError+fmt.Sprintf(format, args...))
	os.Exit(1)
}

// Flags returns the output flags for the logger.
func (l *miniLogger) Flags() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.flag
}

// SetFlags sets the output flags for the logger.
func (l *miniLogger) SetFlags(flag int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flag = flag
}

// Prefix returns the output prefix for the logger.
func (l *miniLogger) Prefix() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.prefix
}

// SetPrefix sets the output prefix for the logger.
func (l *miniLogger) SetPrefix(prefix string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prefix = prefix
}

// SetOutput sets the output destination for the standard logger.
func SetOutput(w io.Writer) {
	std.mu.Lock()
	defer std.mu.Unlock()
	std.out = w
}

// Flags returns the output flags for the standard logger.
func Flags() int {
	return std.Flags()
}

// SetFlags sets the output flags for the standard logger.
func SetFlags(flag int) {
	std.SetFlags(flag)
}

// Prefix returns the output prefix for the standard logger.
func Prefix() string {
	return std.Prefix()
}

// SetPrefix sets the output prefix for the standard logger.
func SetPrefix(prefix string) {
	std.SetPrefix(prefix)
}

// These functions write to the standard logger.

// Debug logs in verbose mode only, using the standard logger.
// Arguments are handled in the manner of fmt.Print.
func Debug(args ...interface{}) {
	if std.Flags()&LDebug == 0 {
		return
	}

	std.Output(2, tagDebug+fmt.Sprintln(args...))
}

// Debugf logs in verbose mode only, using the standard logger.
// Arguments are handled in the manner of fmt.Printf.
func Debugf(format string, args ...interface{}) {
	if std.Flags()&LDebug == 0 {
		return
	}

	std.Output(2, tagDebug+fmt.Sprintf(format, args...))
}

// Info logs to the standard logger.
// Arguments are handled in the manner of fmt.Print.
func Info(args ...interface{}) {
	std.Output(2, tagInfo+fmt.Sprintln(args...))
}

// Infof logs to the standard logger.
// Arguments are handled in the manner of fmt.Printf.
func Infof(format string, args ...interface{}) {
	std.Output(2, tagInfo+fmt.Sprintf(format, args...))
}

// Fatal logs to the standard logger and exits (1) afterwards.
// Arguments are handled in the manner of fmt.Print.
// This should only be used for errors that can't be handled.
func Fatal(args ...interface{}) {
	std.Output(2, tagError+fmt.Sprintln(args...))
	os.Exit(1)
}

// Fatalf logs to the standard logger and exits (1) afterwards.
// Arguments are handled in the manner of fmt.Printf.
// This should only be used for errors that can't be handled.
func Fatalf(format string, args ...interface{}) {
	std.Output(2, tagError+fmt.Sprintf(format, args...))
	os.Exit(1)
}
