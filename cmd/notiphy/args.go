package main

import (
	"flag"
	"strings"
)

// parseArgs parses flags that may appear before *or* after positional
// arguments, and returns the positionals.
//
// The stdlib flag package stops at the first non-flag argument, so
// `notiphy ask "Deploy?" --wait` would silently ignore --wait and every flag
// after it. Since the natural way to write these commands puts the message
// first, we reorder the arguments before handing them to flag.Parse.
func parseArgs(fs *flag.FlagSet, args []string) []string {
	var flags, positional []string

	for i := 0; i < len(args); i++ {
		a := args[i]

		// Everything after "--" is positional by convention.
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}

		if len(a) > 1 && strings.HasPrefix(a, "-") {
			flags = append(flags, a)

			// "-flag=value" carries its own value. "-flag value" does not, so
			// for any non-boolean flag we must pull the following argument
			// across too, or it would be mistaken for a positional.
			if !strings.Contains(a, "=") && !isBoolFlag(fs, a) && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}

		positional = append(positional, a)
	}

	fs.Parse(append(flags, positional...))
	return fs.Args()
}

// isBoolFlag reports whether the named flag is a boolean, which determines
// whether it consumes the next argument.
func isBoolFlag(fs *flag.FlagSet, arg string) bool {
	name := strings.TrimLeft(arg, "-")
	f := fs.Lookup(name)
	if f == nil {
		// Unknown flag: let flag.Parse produce the error rather than guessing
		// that it swallows the next argument.
		return true
	}
	bf, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && bf.IsBoolFlag()
}
