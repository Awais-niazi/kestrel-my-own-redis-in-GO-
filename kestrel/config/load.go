package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Load builds a configuration from the file named in args (if any), the
// environment, and the command-line flags, applying them in that order so
// that later sources win (NFR-7).
//
// The accepted argument forms are the ones operators expect from this class
// of server: a bare path to a config file, --name value, and --name=value.
func Load(args []string) (*Config, error) {
	c := Default()

	path, rest, err := splitConfigPath(args)
	if err != nil {
		return nil, err
	}
	if path != "" {
		if err := c.LoadFile(path); err != nil {
			return nil, err
		}
		c.withLock(func(v *Values) { v.Source = path })
	}
	if err := c.LoadEnv(os.Environ()); err != nil {
		return nil, err
	}
	if err := c.LoadFlags(rest); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// splitConfigPath extracts the config file path from the argument list.
func splitConfigPath(args []string) (path string, rest []string, err error) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--config" || a == "-c":
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("%s requires a file path", a)
			}
			path = args[i+1]
			i++
		case strings.HasPrefix(a, "--config="):
			path = strings.TrimPrefix(a, "--config=")
		case !strings.HasPrefix(a, "-") && path == "" && len(rest) == 0:
			// A leading bare path is the config file.
			path = a
		default:
			rest = append(rest, a)
		}
	}
	return path, rest, nil
}

// LoadFile applies directives from a configuration file.
func LoadFile(path string) (*Config, error) {
	c := Default()
	if err := c.LoadFile(path); err != nil {
		return nil, err
	}
	c.withLock(func(v *Values) { v.Source = path })
	return c, c.Validate()
}

// LoadFile applies directives from path onto c.
func (c *Config) LoadFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		fields, err := splitDirective(sc.Text())
		if err != nil {
			return fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if len(fields) == 0 {
			continue
		}
		if err := c.applyDirective(fields); err != nil {
			return fmt.Errorf("%s:%d: %w", path, line, err)
		}
	}
	return sc.Err()
}

// LoadEnv applies KESTREL_* variables. The suffix is the parameter name with
// dashes turned into underscores, e.g. KESTREL_MAXMEMORY_POLICY.
func (c *Config) LoadEnv(environ []string) error {
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, "KESTREL_") {
			continue
		}
		param := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(name, "KESTREL_"), "_", "-"))
		if _, known := specs[param]; !known {
			return fmt.Errorf("unknown parameter %q from environment variable %s", param, name)
		}
		if err := c.applyRaw(param, value); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// LoadFlags applies --name value and --name=value arguments.
func (c *Config) LoadFlags(args []string) error {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			return fmt.Errorf("unexpected argument %q", a)
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		name = strings.ToLower(name)
		if !hasValue {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				// A valueless flag is a boolean set to yes.
				value = "yes"
			} else {
				value = args[i+1]
				i++
			}
		}
		if name == "rename-command" {
			fields, err := splitDirective("rename-command " + value)
			if err != nil {
				return err
			}
			if err := c.applyDirective(fields); err != nil {
				return err
			}
			continue
		}
		if _, known := specs[name]; !known {
			return fmt.Errorf("unknown option --%s", name)
		}
		if err := c.applyRaw(name, value); err != nil {
			return fmt.Errorf("--%s: %w", name, err)
		}
	}
	return nil
}

// applyDirective handles one config-file line, including the directives that
// are not simple name/value pairs.
func (c *Config) applyDirective(fields []string) error {
	name := strings.ToLower(fields[0])
	switch name {
	case "rename-command":
		if len(fields) != 3 {
			return fmt.Errorf("rename-command takes exactly two arguments")
		}
		c.withLock(func(v *Values) {
			v.RenamedCommands[strings.ToUpper(fields[1])] = strings.ToUpper(fields[2])
		})
		return nil
	case "include":
		if len(fields) != 2 {
			return fmt.Errorf("include takes exactly one argument")
		}
		return c.LoadFile(fields[1])
	case "save":
		// Accepted and ignored: snapshot scheduling is expressed with
		// snapshot-interval, so a copied redis.conf does not fail to load.
		return nil
	}
	if len(fields) < 2 {
		return fmt.Errorf("directive %q requires a value", name)
	}
	if _, known := specs[name]; !known {
		return fmt.Errorf("unknown directive %q", name)
	}
	return c.applyRaw(name, strings.Join(fields[1:], " "))
}

// applyRaw sets a parameter regardless of whether CONFIG SET may change it,
// which is the difference between load time and run time.
func (c *Config) applyRaw(name, value string) error {
	s, ok := specs[name]
	if !ok {
		return fmt.Errorf("unknown parameter %q", name)
	}
	return c.update(func(v *Values) error { return s.set(v, value) })
}

// splitDirective tokenizes a config line, honouring quotes and dropping
// comments.
func splitDirective(line string) ([]string, error) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] == '#' {
		return nil, nil
	}
	var (
		fields []string
		cur    strings.Builder
		quote  byte
		has    bool
	)
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case quote != 0:
			if ch == '\\' && i+1 < len(line) && quote == '"' {
				i++
				cur.WriteByte(unescapeConf(line[i]))
				continue
			}
			if ch == quote {
				quote = 0
				continue
			}
			cur.WriteByte(ch)
		case ch == '"' || ch == '\'':
			quote = ch
			has = true
		case ch == '#':
			// A comment starts only at a token boundary, so that values
			// containing '#' inside quotes survive.
			if cur.Len() == 0 && !has {
				i = len(line)
				continue
			}
			cur.WriteByte(ch)
		case ch == ' ' || ch == '\t':
			if cur.Len() > 0 || has {
				fields = append(fields, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteByte(ch)
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unbalanced quotes")
	}
	if cur.Len() > 0 || has {
		fields = append(fields, cur.String())
	}
	return fields, nil
}

func unescapeConf(c byte) byte {
	switch c {
	case 'n':
		return '\n'
	case 'r':
		return '\r'
	case 't':
		return '\t'
	default:
		return c
	}
}
