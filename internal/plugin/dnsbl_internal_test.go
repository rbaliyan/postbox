package plugin

import (
	"net"
	"testing"
)

func TestReverseIPv4(t *testing.T) {
	tests := []struct {
		ip   string
		want string
	}{
		{"1.2.3.4", "4.3.2.1"},
		{"127.0.0.1", "1.0.0.127"},
		{"192.168.1.100", "100.1.168.192"},
	}
	for _, tt := range tests {
		ip := net.ParseIP(tt.ip).To4()
		got := reverseIPv4(ip)
		if got != tt.want {
			t.Errorf("reverseIPv4(%s) = %s, want %s", tt.ip, got, tt.want)
		}
	}
}
