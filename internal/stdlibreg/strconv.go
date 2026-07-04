package stdlibreg

import "strconv"

func init() {
	Register(&Package{Name: "strconv", Symbols: map[string]any{
		"Atoi":        strconv.Atoi,
		"Itoa":        strconv.Itoa,
		"ParseFloat":  strconv.ParseFloat,
		"ParseBool":   strconv.ParseBool,
		"FormatFloat": strconv.FormatFloat,
		"Quote":       strconv.Quote,
		"Unquote":     strconv.Unquote,
	}})
}
