package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLoadPackages(t *testing.T) {
	pkgs, err := loadPackages(".", []string{"./example/in"})
	if err != nil {
		t.Fatalf("error loading packages: %s", err)
	}

	assert := assert.New(t)
	assert.True(len(pkgs) > 0, "pkgs should not be empty")
}

func TestGetMessages(t *testing.T) {
	pkgs, err := loadPackages(".", []string{"./example/in"})
	if err != nil {
		t.Fatalf("error loading packages: %s", err)
	}

	msgs, enums := getProtobufTypes(pkgs, "")

	for _, msg := range msgs {
		t.Logf("message: %s", msg.Name)
	}

	for _, enum := range enums {
		t.Logf("enum: %s", enum.Name)
	}
}
