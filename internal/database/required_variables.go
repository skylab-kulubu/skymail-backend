package database

// ContractVariable is a Required variable from the sending service's
// contract, as the repo declares it beside the Template key.
type ContractVariable struct {
	// The variable, as the body reaches it with .Name.
	Name string `json:"name"`
	// Why the mail cannot do without it, a sentence the repo declares. Null when the Template seed sent the name alone.
	Reason *string `json:"reason"`
}

// ContractVariables is a template's contract set, sorted by name byte by
// byte, each name once. The database keeps it as a JSON array.
type ContractVariables []ContractVariable

// Names are the set's names, in its order.
func (c ContractVariables) Names() []string {
	names := make([]string, len(c))
	for i, variable := range c {
		names[i] = variable.Name
	}
	return names
}

// Find is the entry named name, if the set has one.
func (c ContractVariables) Find(name string) (ContractVariable, bool) {
	for _, variable := range c {
		if variable.Name == name {
			return variable, true
		}
	}
	return ContractVariable{}, false
}
