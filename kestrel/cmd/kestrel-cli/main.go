// Command kestrel-cli is an interactive client for a Kestrel or any
// RESP-speaking server.
package main

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"kestrel/command"
	"kestrel/resp"
)

type options struct {
	host     string
	port     int
	password string
	user     string
	db       int
	useTLS   bool
	insecure bool
	timeout  time.Duration
	repeat   int
	args     []string
}

func main() {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "kestrel-cli: %v\n", err)
		os.Exit(1)
	}
	if err := run(opts); err != nil {
		fmt.Fprintf(os.Stderr, "kestrel-cli: %v\n", err)
		os.Exit(1)
	}
}

func parseArgs(argv []string) (*options, error) {
	o := &options{host: "127.0.0.1", port: 6380, timeout: 10 * time.Second, repeat: 1}
	i := 0
	for ; i < len(argv); i++ {
		a := argv[i]
		next := func() (string, error) {
			if i+1 >= len(argv) {
				return "", fmt.Errorf("%s requires a value", a)
			}
			i++
			return argv[i], nil
		}
		var err error
		switch a {
		case "-h", "--host":
			o.host, err = next()
		case "-p", "--port":
			var v string
			if v, err = next(); err == nil {
				o.port, err = strconv.Atoi(v)
			}
		case "-a", "--pass":
			o.password, err = next()
		case "--user":
			o.user, err = next()
		case "-n", "--db":
			var v string
			if v, err = next(); err == nil {
				o.db, err = strconv.Atoi(v)
			}
		case "-r", "--repeat":
			var v string
			if v, err = next(); err == nil {
				o.repeat, err = strconv.Atoi(v)
			}
		case "--tls":
			o.useTLS = true
		case "--insecure":
			o.insecure = true
		case "--help":
			usage()
			os.Exit(0)
		case "--version":
			fmt.Printf("kestrel-cli %s\n", command.Version)
			os.Exit(0)
		default:
			if strings.HasPrefix(a, "-") {
				return nil, fmt.Errorf("unknown option %q", a)
			}
			o.args = argv[i:]
			return o, nil
		}
		if err != nil {
			return nil, err
		}
	}
	return o, nil
}

func usage() {
	fmt.Printf(`kestrel-cli %s

Usage:
  kestrel-cli [options] [command [args...]]

Options:
  -h, --host <host>    Server hostname (default 127.0.0.1)
  -p, --port <port>    Server port (default 6380)
  -a, --pass <pass>    Password for AUTH
      --user <name>    Username for AUTH
  -n, --db <index>     Database to SELECT (default 0)
  -r, --repeat <n>     Run the command n times
      --tls            Connect with TLS
      --insecure       Skip TLS certificate verification

With no command, kestrel-cli starts an interactive session.
`, command.Version)
}

type client struct {
	conn net.Conn
	rd   *resp.ReplyReader
	out  *bufio.Writer
}

func dial(o *options) (*client, error) {
	addr := net.JoinHostPort(o.host, strconv.Itoa(o.port))
	var (
		conn net.Conn
		err  error
	)
	if o.useTLS {
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: o.timeout}, "tcp", addr,
			&tls.Config{InsecureSkipVerify: o.insecure, ServerName: o.host})
	} else {
		conn, err = net.DialTimeout("tcp", addr, o.timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("could not connect to %s: %w", addr, err)
	}
	return &client{conn: conn, rd: resp.NewReplyReader(conn), out: bufio.NewWriter(conn)}, nil
}

func (c *client) do(args ...string) (resp.Value, error) {
	raw := make([][]byte, len(args))
	for i, a := range args {
		raw[i] = []byte(a)
	}
	c.out.Write(resp.EncodeCommand(nil, raw...))
	if err := c.out.Flush(); err != nil {
		return resp.Value{}, err
	}
	return c.rd.ReadReply()
}

func run(o *options) error {
	c, err := dial(o)
	if err != nil {
		return err
	}
	defer c.conn.Close()

	if o.password != "" {
		auth := []string{"AUTH", o.password}
		if o.user != "" {
			auth = []string{"AUTH", o.user, o.password}
		}
		v, err := c.do(auth...)
		if err != nil {
			return err
		}
		if v.IsError() {
			return fmt.Errorf("%s", v.Str)
		}
	}
	if o.db != 0 {
		if v, err := c.do("SELECT", strconv.Itoa(o.db)); err != nil {
			return err
		} else if v.IsError() {
			return fmt.Errorf("%s", v.Str)
		}
	}

	if len(o.args) > 0 {
		for i := 0; i < o.repeat; i++ {
			v, err := c.do(o.args...)
			if err != nil {
				return err
			}
			fmt.Println(format(v, 0))
		}
		return nil
	}
	return repl(c, o)
}

func repl(c *client, o *options) error {
	prompt := fmt.Sprintf("%s:%d> ", o.host, o.port)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for {
		fmt.Print(prompt)
		if !in.Scan() {
			fmt.Println()
			return in.Err()
		}
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		args, err := splitLine(line)
		if err != nil {
			fmt.Println("(error) " + err.Error())
			continue
		}
		if len(args) == 0 {
			continue
		}
		switch strings.ToLower(args[0]) {
		case "exit", "quit":
			return nil
		}
		v, err := c.do(args...)
		if err != nil {
			if err == io.EOF {
				return fmt.Errorf("server closed the connection")
			}
			return err
		}
		fmt.Println(format(v, 0))
	}
}

// splitLine tokenizes an input line the way the server tokenizes an inline
// command, so that quoting behaves the same in both places.
func splitLine(line string) ([]string, error) {
	var (
		out   []string
		cur   strings.Builder
		quote byte
		has   bool
	)
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case quote != 0:
			if ch == '\\' && quote == '"' && i+1 < len(line) {
				i++
				switch line[i] {
				case 'n':
					cur.WriteByte('\n')
				case 't':
					cur.WriteByte('\t')
				case 'r':
					cur.WriteByte('\r')
				default:
					cur.WriteByte(line[i])
				}
				continue
			}
			if ch == quote {
				quote = 0
				continue
			}
			cur.WriteByte(ch)
		case ch == '"' || ch == '\'':
			quote, has = ch, true
		case ch == ' ' || ch == '\t':
			if cur.Len() > 0 || has {
				out = append(out, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteByte(ch)
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unbalanced quotes in request")
	}
	if cur.Len() > 0 || has {
		out = append(out, cur.String())
	}
	return out, nil
}

// format renders a reply the way redis-cli does, so that output is familiar.
func format(v resp.Value, depth int) string {
	switch v.Kind {
	case resp.KindSimple:
		return string(v.Str)
	case resp.KindError:
		return "(error) " + string(v.Str)
	case resp.KindInt:
		return "(integer) " + strconv.FormatInt(v.Int, 10)
	case resp.KindBulk, resp.KindVerbatim, resp.KindBigNum:
		return strconv.Quote(string(v.Str))
	case resp.KindNull, resp.KindNullArray:
		return "(nil)"
	case resp.KindBool:
		if v.Bool {
			return "(true)"
		}
		return "(false)"
	case resp.KindDouble:
		return "(double) " + resp.FormatDouble(v.Float)
	case resp.KindArray, resp.KindSet, resp.KindPush, resp.KindMap:
		if len(v.Elems) == 0 {
			return "(empty array)"
		}
		var b strings.Builder
		for i, e := range v.Elems {
			if i > 0 {
				b.WriteString("\n")
				b.WriteString(strings.Repeat("   ", depth))
			}
			fmt.Fprintf(&b, "%d) %s", i+1, format(e, depth+1))
		}
		return b.String()
	default:
		return ""
	}
}
