package command

import (
	"strconv"
	"strings"

	"kestrel/resp"
)

func init() {
	register(&Descriptor{
		Name: "ACL", Arity: -2, Flags: Readonly | Admin | Loading | Stale,
		Categories: []string{"admin", "slow", "dangerous"},
		Summary:    "A container for access control commands.",
		Subcommands: map[string]*Descriptor{
			"WHOAMI": {Arity: 2, Flags: Readonly | Fast | Loading | Stale,
				Summary: "Returns the authenticated username.", Handler: cmdACLWhoAmI},
			"LIST": {Arity: 2, Flags: Readonly | Admin | Loading | Stale,
				Summary: "Lists the users and their rules.", Handler: cmdACLList},
			"USERS": {Arity: 2, Flags: Readonly | Admin | Loading | Stale,
				Summary: "Lists the usernames.", Handler: cmdACLUsers},
			"GETUSER": {Arity: 3, Flags: Readonly | Admin | Loading | Stale,
				Summary: "Describes one user.", Handler: cmdACLGetUser},
			"SETUSER": {Arity: -3, Flags: Readonly | Admin | Loading | Stale,
				Summary: "Creates or changes a user.", Handler: cmdACLSetUser},
			"DELUSER": {Arity: -3, Flags: Readonly | Admin | Loading | Stale,
				Summary: "Removes users.", Handler: cmdACLDelUser},
			"CAT": {Arity: -2, Flags: Readonly | Admin | Loading | Stale,
				Summary: "Lists categories, or the commands in one.", Handler: cmdACLCat},
			"GENPASS": {Arity: -2, Flags: Readonly | Fast | Loading | Stale,
				Summary: "Returns a random password.", Handler: cmdACLGenPass},
			"HELP": {Arity: 2, Flags: Readonly | Fast | Loading | Stale,
				Summary: "Shows helpful text.", Handler: cmdACLHelp},
		},
	})
}

func cmdACLWhoAmI(c *Ctx) resp.Value { return resp.BulkString(c.Client.User) }

func cmdACLList(c *Ctx) resp.Value {
	users := c.Host.ACL().Users()
	out := make([]resp.Value, 0, len(users))
	for _, u := range users {
		out = append(out, resp.BulkString("user "+u.Name+" "+u.Describe()))
	}
	return resp.ArrayOf(out)
}

func cmdACLUsers(c *Ctx) resp.Value {
	names := c.Host.ACL().Names()
	out := make([]resp.Value, len(names))
	for i, n := range names {
		out[i] = resp.BulkString(n)
	}
	return resp.ArrayOf(out)
}

func cmdACLGetUser(c *Ctx) resp.Value {
	u := c.Host.ACL().User(string(c.Arg(2)))
	if u == nil {
		return resp.NullArray()
	}
	flags, passwords, keys, channels, commands := u.Snapshot()
	return resp.Map([]resp.Value{
		resp.BulkString("flags"), stringArray(flags),
		resp.BulkString("passwords"), stringArray(passwords),
		resp.BulkString("commands"), resp.BulkString(commands),
		resp.BulkString("keys"), resp.BulkString(strings.Join(keys, " ")),
		resp.BulkString("channels"), resp.BulkString(strings.Join(channels, " ")),
	})
}

func stringArray(xs []string) resp.Value {
	out := make([]resp.Value, len(xs))
	for i, x := range xs {
		out[i] = resp.BulkString(x)
	}
	return resp.ArrayOf(out)
}

func cmdACLSetUser(c *Ctx) resp.Value {
	name := string(c.Arg(2))
	if strings.ContainsAny(name, " \n\r") {
		return resp.Err("ERR Usernames can't contain spaces or null characters")
	}
	rules := make([]string, 0, c.Len()-3)
	for i := 3; i < c.Len(); i++ {
		rules = append(rules, string(c.Arg(i)))
	}
	if err := c.Host.ACL().SetUser(name, rules); err != nil {
		return resp.Err("ERR " + err.Error())
	}
	return resp.OK()
}

func cmdACLDelUser(c *Ctx) resp.Value {
	names := make([]string, 0, c.Len()-2)
	for i := 2; i < c.Len(); i++ {
		names = append(names, string(c.Arg(i)))
	}
	n, err := c.Host.ACL().DelUser(names)
	if err != nil {
		return resp.Err("ERR " + err.Error())
	}
	return resp.Int(int64(n))
}

func cmdACLCat(c *Ctx) resp.Value {
	a := c.Host.ACL()
	if c.Len() == 2 {
		return stringArray(a.Categories())
	}
	if c.Len() != 3 {
		return errWrongArgs("acl|cat")
	}
	commands, known := a.CommandsIn(strings.ToLower(string(c.Arg(2))))
	if !known {
		return resp.Err("ERR Unknown ACL cat '" + string(c.Arg(2)) + "'")
	}
	return stringArray(commands)
}

func cmdACLGenPass(c *Ctx) resp.Value {
	bits := 256
	if c.Len() == 3 {
		n, err := strconv.Atoi(string(c.Arg(2)))
		if err != nil || n <= 0 || n > 4096 {
			return resp.Err("ERR ACL GENPASS argument must be the number of bits for the output password, a positive number up to 4096")
		}
		bits = n
	} else if c.Len() != 2 {
		return errWrongArgs("acl|genpass")
	}
	p, err := genPassword(bits)
	if err != nil {
		return resp.Err("ERR " + err.Error())
	}
	return resp.BulkString(p)
}

func cmdACLHelp(c *Ctx) resp.Value {
	lines := []string{
		"ACL <subcommand>",
		"WHOAMI                -- Return the authenticated username.",
		"LIST                  -- List users and their rules.",
		"USERS                 -- List usernames.",
		"GETUSER <user>        -- Describe one user.",
		"SETUSER <user> [rule ...] -- Create or change a user.",
		"DELUSER <user> [...]  -- Remove users.",
		"CAT [category]        -- List categories, or the commands in one.",
		"GENPASS [bits]        -- Return a random password.",
	}
	out := make([]resp.Value, len(lines))
	for i, l := range lines {
		out[i] = resp.Simple(l)
	}
	return resp.ArrayOf(out)
}
