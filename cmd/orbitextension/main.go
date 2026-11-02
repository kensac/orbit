// Command orbitextension validates proposed extension packages. It never runs them.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/kensac/orbit/internal/extensions"
)

func run(args []string, out io.Writer) error {
	if len(args) != 2 || args[0] != "validate" {
		return fmt.Errorf("usage: orbitextension validate <package-directory> (validation only; does not install or execute)")
	}
	p, err := extensions.ValidateDir(args[1])
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(p)
}
func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
