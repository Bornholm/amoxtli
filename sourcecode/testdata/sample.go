package sample

import "fmt"

// Greeter formats greetings.
//
// It is safe for concurrent use.
type Greeter struct {
	prefix string
}

// NewGreeter returns a Greeter using the given prefix.
func NewGreeter(prefix string) *Greeter {
	return &Greeter{prefix: prefix}
}

// Greet says hello to the given name.
func (g *Greeter) Greet(name string) string {
	return fmt.Sprintf("%s %s", g.prefix, name)
}

// Farewell says goodbye.
func Farewell(name string) string {
	return "goodbye " + name
}
