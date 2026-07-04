package stdlibreg

import "sort"

func init() {
	Register(&Package{Name: "sort", Symbols: map[string]any{
		"Strings":          sort.Strings,
		"Ints":             sort.Ints,
		"Float64s":         sort.Float64s,
		"StringsAreSorted": sort.StringsAreSorted,
		"SearchStrings":    sort.SearchStrings,
	}})
}
