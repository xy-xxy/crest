package config

import (
	"fmt"
	"testing"
)

func TestBlockActuatorInvalidValues(t *testing.T) {
	for _, value := range []string{"", "false", "maybe", "on off"} {
		for _, scope := range []string{"http", "server"} {
			t.Run("nginx/"+scope+"/"+value, func(t *testing.T) {
				directive := "block_actuator " + value + ";"
				src := "http { " + directive + " server { listen 8080; } }"
				if scope == "server" {
					src = "http { server { listen 8080; " + directive + " } }"
				}
				ds, err := Parse(src)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := Load(ds); err == nil {
					t.Fatal("invalid block_actuator value accepted")
				}
			})
		}
	}
	for _, value := range []string{"maybe", "123", "[false]", "{enabled: false}"} {
		for _, scope := range []string{"http", "server"} {
			t.Run("yaml/"+scope+"/"+value, func(t *testing.T) {
				src := fmt.Sprintf("http:\n  block_actuator: %s\n  servers:\n    - listen: [8080]\n", value)
				if scope == "server" {
					src = fmt.Sprintf("http:\n  servers:\n    - listen: [8080]\n      block_actuator: %s\n", value)
				}
				if _, err := LoadYAML([]byte(src)); err == nil {
					t.Fatal("invalid block_actuator value accepted")
				}
			})
		}
	}
}
