package command

import (
	"strings"

	"kestrel/engine"
	"kestrel/resp"
)

// Error replies. The text of these matters: clients match on the leading
// code word, and some (notably WRONGTYPE and MOVED) are parsed by libraries.
var (
	errWrongType    = resp.Err("WRONGTYPE Operation against a key holding the wrong kind of value")
	errNotInteger   = resp.Err("ERR value is not an integer or out of range")
	errNotFloat     = resp.Err("ERR value is not a valid float")
	errOverflow     = resp.Err("ERR increment or decrement would overflow")
	errSyntax       = resp.Err("ERR syntax error")
	errNoSuchKey    = resp.Err("ERR no such key")
	errOutOfRange   = resp.Err("ERR offset is out of range")
	errIndexRange   = resp.Err("ERR value is out of range, must be positive")
	errNoAuth       = resp.Err("NOAUTH Authentication required.")
	errAuthNotSet   = resp.Err("ERR Client sent AUTH, but no password is set. Did you mean AUTH <username> <password>?")
	errAuthFailed   = resp.Err("WRONGPASS invalid username-password pair or user is disabled.")
	errReadOnly     = resp.Err("READONLY You can't write against a read only replica.")
	errOOM          = resp.Err("OOM command not allowed when used memory > 'maxmemory'.")
	errLoading      = resp.Err("LOADING Kestrel is loading the dataset in memory")
	errStringTooBig = resp.Err("ERR string exceeds maximum allowed size (proto-max-bulk-len)")
	errDBIndex      = resp.Err("ERR DB index is out of range")
	errExpireTime   = resp.Err("ERR invalid expire time")
)

// errWrongArgs is the reply for an arity violation.
func errWrongArgs(name string) resp.Value {
	return resp.Err("ERR wrong number of arguments for '" + strings.ToLower(name) + "' command")
}

// errUnknownCommand mirrors the reference implementation's format, which
// echoes the arguments so that a typo is obvious from the error alone.
func errUnknownCommand(name []byte, args [][]byte) resp.Value {
	var b strings.Builder
	b.WriteString("ERR unknown command '")
	b.Write(name)
	b.WriteString("', with args beginning with: ")
	for i, a := range args {
		if i >= 20 {
			break
		}
		b.WriteString("'")
		b.Write(a)
		b.WriteString("', ")
	}
	return resp.Err(b.String())
}

// errUnknownSubcommand is the reply for an unrecognized subcommand.
func errUnknownSubcommand(parent string, sub []byte) resp.Value {
	return resp.Err("ERR Unknown subcommand or wrong number of arguments for '" +
		string(sub) + "'. Try " + parent + " HELP.")
}

// errDisabledCommand explains a command removed by rename-command, rather
// than letting it look like a typo (FR-7.4).
func errDisabledCommand(name []byte) resp.Value {
	return resp.Err("ERR unknown command '" + string(name) +
		"': this command has been disabled by configuration")
}

// errUnsupported is the reply for a command that exists in the reference
// implementation but is deliberately not in this version. Making the
// omission legible rather than generic is the mitigation for R4 and R7.
func errUnsupported(name, adr string) resp.Value {
	return resp.Err("ERR '" + name + "' is not supported in this version of Kestrel. " +
		"See the compatibility matrix in the documentation (" + adr + ").")
}

// engineError maps an engine error onto its wire reply.
func engineError(err error) resp.Value {
	switch err {
	case nil:
		return resp.Value{}
	case engine.ErrWrongType:
		return errWrongType
	case engine.ErrNotInteger:
		return errNotInteger
	case engine.ErrNotFloat:
		return errNotFloat
	case engine.ErrOverflow:
		return errOverflow
	case engine.ErrOutOfRange:
		return errOutOfRange
	case engine.ErrNoSuchKey:
		return errNoSuchKey
	case engine.ErrValueTooLarge:
		return errStringTooBig
	default:
		return resp.Err("ERR " + err.Error())
	}
}

// errMisconf refuses a write because the append log is not taking them.
//
// The name matches the reference implementation's error for the same
// situation, so existing client retry logic recognises it. The reason is
// included because "MISCONF" on its own has sent many people to the wrong
// part of their configuration.
func errMisconf(err error) resp.Value {
	return resp.Err("MISCONF Errors writing to the append log: " + err.Error() +
		". Writes are refused until it recovers.")
}

// errNotEnoughReplicas refuses a write because min-replicas-to-write is not
// satisfied. The counts are in the message: "NOREPLICAS" alone leaves an
// operator to go and find out how far short they are.
func errNotEnoughReplicas(have, need int) resp.Value {
	return resp.Err("NOREPLICAS Not enough good replicas to write. " +
		itoa(have) + " in sync, " + itoa(need) + " required.")
}

// errSubscriberMode refuses a command a RESP2 subscriber may not send.
func errSubscriberMode(name string) resp.Value {
	return resp.Err("ERR Can't execute '" + strings.ToLower(name) +
		"': only (P|S)SUBSCRIBE / (P|S)UNSUBSCRIBE / PING / QUIT / RESET are " +
		"allowed in this context")
}

// errTimeout refuses a blocking command whose timeout argument is unusable.
var errTimeout = resp.Err("ERR timeout is not a float or out of range")
