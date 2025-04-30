package main

import (
	"log"
	"strings"
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

func TestIgnoredField(t *testing.T) {
	pkgs, err := loadPackages(".", []string{"./example/in"})
	if err != nil {
		t.Fatalf("error loading packages: %s", err)
	}

	msgs, _ := getProtobufTypes(pkgs, "")

	var pkgForTest *message
	for _, v := range msgs {
		log.Println(v.Name)
		if strings.Trim(v.Name, " ") == "EventFieldItem" {
			pkgForTest = v
			break
		}
	}

	assert.NotNil(t, pkgForTest, "Could not find test struct EventFieldItem")

	for _, v := range pkgForTest.Fields {
		assert.NotEqual(t, v.Name, "ignore_this_field", "IgnoreThisField should be ignored from parsing")
	}
}
