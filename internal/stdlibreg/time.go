package stdlibreg

import "time"

func init() {
	Register(&Package{Name: "time", Symbols: map[string]any{
		"Now":         time.Now,
		"Since":       time.Since,
		"Sleep":       time.Sleep,
		"Parse":       time.Parse,
		"Unix":        time.Unix,
		"Nanosecond":  time.Nanosecond,
		"Microsecond": time.Microsecond,
		"Millisecond": time.Millisecond,
		"Second":      time.Second,
		"Minute":      time.Minute,
		"Hour":        time.Hour,
		"RFC3339":     time.RFC3339,
		"DateOnly":    time.DateOnly,
		"TimeOnly":    time.TimeOnly,
		"DateTime":    time.DateTime,
	}})
}
