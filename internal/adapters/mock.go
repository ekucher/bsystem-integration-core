package adapters

import "context"

type Mock struct {
	AdapterInfo   Info
	AdapterHealth Health
}

func (m Mock) Info() Info {
	return m.AdapterInfo
}

func (m Mock) Health(context.Context) Health {
	return m.AdapterHealth
}
