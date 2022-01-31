package example

import "time"

//go:generate go run github.com/jakoblorz/go-gentoolkit/cmd/go-gen-getter -type=ExampleStruct

type ExampleStruct struct {
	Field1 time.Time
	Field2 string
}
