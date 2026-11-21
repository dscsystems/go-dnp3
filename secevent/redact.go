package secevent

import (
	"bytes"

	"github.com/dscsystems/go-dnp3/internal/app"
)

// secretBearing reports whether an application fragment may carry a
// credential, so its octets must not be kept on an event: a file
// authentication's user name and password (g70v2), the key a file open or
// delete presents or an outstation hands back (g70v3, g70v4), and Secure
// Authentication's challenges, MACs and wrapped keys (g120).
//
// It errs on the side of dropping. A fragment that does not parse is judged
// by its function code and by whether its octets could hold one of those
// object headers at all.
func secretBearing(raw []byte) bool {
	frag, err := app.ParseFragment(nil, raw)
	if len(raw) >= 2 {
		switch app.FuncCode(raw[1]) {
		case app.FuncOpenFile, app.FuncDeleteFile, app.FuncAuthenticateFile,
			app.FuncAuthRequest, app.FuncAuthRequestNoAck, app.FuncAuthResponse:
			return true
		}
	}
	for _, o := range frag.Objects {
		if o.Group == 120 || (o.Group == 70 && o.Variation >= 2 && o.Variation <= 4) {
			return true
		}
	}
	if err != nil {
		// Where parsing stopped is not where the objects end: look for
		// anything shaped like one of those headers in what is left.
		for v := byte(2); v <= 4; v++ {
			if bytes.Contains(raw, []byte{70, v}) {
				return true
			}
		}
		for v := byte(1); v <= 15; v++ {
			if bytes.Contains(raw, []byte{120, v}) {
				return true
			}
		}
	}
	return false
}
