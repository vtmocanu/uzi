// Command uzi is the terminal control surface for the uzi factory, driven
// identically by humans (tables on a TTY) and agents (--json, documented exit
// codes). This is the M3 skeleton: the full noun-verb tree is wired to cobra
// and renders --help; read verbs run against a Client interface (real HTTP impl
// stubbed until M7), and mutating/auth verbs are stubs.
package main

import "os"

func main() {
	os.Exit(Main(DefaultEnv(), os.Args[1:]))
}
