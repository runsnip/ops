package standard

import (
	"errors"

	"github.com/runsnip/ops/shim"
)

// Adoption is a draft stack file and its report.
type Adoption struct{ InfraDir, Stack, Report string }

// Adopt is written in adopt_impl.go.
// write lets it put runsnip.draft (the standard shim) beside the compose file.
func Adopt(dir string, write bool) (Adoption, error) { return adopt(dir, write) }

var errNoCompose = errors.New("no compose file found")

func shimScript() []byte { return shim.Script }
