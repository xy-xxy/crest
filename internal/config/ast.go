package config

// Directive is one parsed configuration entry. A simple directive
// ("listen 80;") has Block == nil; a block directive ("server { ... }")
// carries its children in Block, which is non-nil even when empty.
type Directive struct {
	Name  string
	Args  []string
	Block []Directive
	Line  int
}
