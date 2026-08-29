// Command valmind is the Valmin panel daemon.
//
// Startup sequence: 10 §2 (validation gate) then 12 §9.1 (lease, job sweep,
// reconciliation, resume intents, streams).
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "valmind: startup gate not implemented yet (10 §2, WP-M1-06)")
	os.Exit(1)
}
