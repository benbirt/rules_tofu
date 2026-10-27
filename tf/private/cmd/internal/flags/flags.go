// Package flags holds command-line flag helpers shared by rules_tofu's Go
// tools.
package flags

import (
	"flag"
	"strings"
)

// stringList is a flag.Value that appends each occurrence of its flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// StringList defines a repeatable string flag on the default flag set (e.g.
// --var-file can appear multiple times), like flag.String does for a single
// value.
func StringList(name, usage string) *[]string {
	sl := new(stringList)
	flag.Var(sl, name, usage)
	return (*[]string)(sl)
}
