package stdlibreg

import "math"

func init() {
	Register(&Package{Name: "math", Symbols: map[string]any{
		"Abs":    math.Abs,
		"Ceil":   math.Ceil,
		"Floor":  math.Floor,
		"Round":  math.Round,
		"Sqrt":   math.Sqrt,
		"Pow":    math.Pow,
		"Log":    math.Log,
		"Log2":   math.Log2,
		"Log10":  math.Log10,
		"Max":    math.Max,
		"Min":    math.Min,
		"Mod":    math.Mod,
		"Pi":     math.Pi,
		"Inf":    math.Inf,
		"NaN":    math.NaN,
		"MaxInt": math.MaxInt,
		"MinInt": math.MinInt,
	}})
}
