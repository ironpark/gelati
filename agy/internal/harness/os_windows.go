package harness

import "os"

// terminate kills the process; Windows has no SIGTERM, and Popen.terminate
// is TerminateProcess there too.
func terminate(p *os.Process) error { return p.Kill() }
