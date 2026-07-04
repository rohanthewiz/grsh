package stdlibreg

import (
	"github.com/rohanthewiz/logger"
	"github.com/rohanthewiz/serr"
)

// The author's own error/logging libraries, available to scripts.
func init() {
	Register(&Package{Name: "serr", Symbols: map[string]any{
		"New":           serr.New,
		"NewF":          serr.NewF,
		"Wrap":          serr.Wrap,
		"WrapF":         serr.WrapF,
		"F":             serr.F,
		"StringFromErr": serr.StringFromErr,
	}})
	Register(&Package{Name: "logger", Symbols: map[string]any{
		"Info":   logger.Info,
		"Debug":  logger.Debug,
		"Warn":   logger.Warn,
		"Err":    logger.Err,
		"LogErr": logger.LogErr,
	}})
}
