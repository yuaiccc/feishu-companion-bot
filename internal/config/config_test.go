package config

import (
	"os"
	"testing"
)

func TestGetEnv(t *testing.T) {
	os.Setenv("TEST_KEY", "test_value")
	defer os.Unsetenv("TEST_KEY")

	if got := getEnv("TEST_KEY", "default"); got != "test_value" {
		t.Errorf("getEnv = %q, want %q", got, "test_value")
	}
	if got := getEnv("NONEXISTENT", "default"); got != "default" {
		t.Errorf("getEnv = %q, want %q", got, "default")
	}
}

func TestGetEnvBool(t *testing.T) {
	os.Setenv("TEST_BOOL_TRUE", "true")
	os.Setenv("TEST_BOOL_FALSE", "false")
	defer os.Unsetenv("TEST_BOOL_TRUE")
	defer os.Unsetenv("TEST_BOOL_FALSE")

	if got := getEnvBool("TEST_BOOL_TRUE", false); !got {
		t.Errorf("getEnvBool true = %v, want true", got)
	}
	if got := getEnvBool("TEST_BOOL_FALSE", true); got {
		t.Errorf("getEnvBool false = %v, want false", got)
	}
	if got := getEnvBool("NONEXISTENT", true); !got {
		t.Errorf("getEnvBool default = %v, want true", got)
	}
}

func TestGetEnvInt(t *testing.T) {
	os.Setenv("TEST_INT", "42")
	defer os.Unsetenv("TEST_INT")

	if got := getEnvInt("TEST_INT", 0); got != 42 {
		t.Errorf("getEnvInt = %d, want 42", got)
	}
	if got := getEnvInt("NONEXISTENT", 99); got != 99 {
		t.Errorf("getEnvInt default = %d, want 99", got)
	}
}

func TestSplitEnv(t *testing.T) {
	if got := splitEnv("a,b,c", ","); len(got) != 3 {
		t.Errorf("splitEnv len = %d, want 3", len(got))
	}
	if got := splitEnv("", ","); len(got) != 0 {
		t.Errorf("splitEnv empty len = %d, want 0", len(got))
	}
	if got := splitEnv("a, b , c", ","); len(got) != 3 {
		t.Errorf("splitEnv with spaces len = %d, want 3", len(got))
	}
}

func TestNormalizeJDBCMySQLDSN(t *testing.T) {
	got := normalizeJDBCMySQLDSN("jdbc:mysql://127.0.0.1:2881/shuye_chat")
	want := "root@tcp(127.0.0.1:2881)/shuye_chat?parseTime=true&charset=utf8mb4"
	if got != want {
		t.Errorf("normalizeJDBCMySQLDSN = %q, want %q", got, want)
	}
}
