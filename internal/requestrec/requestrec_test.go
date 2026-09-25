package requestrec

import "testing"

func TestTerminalStatusClassification(t *testing.T) {
	for _, status := range []string{StatusSuccess, StatusUpstreamError, StatusGatewayTimeout, StatusStreamBroken, StatusClientDisconnect, StatusQuotaRejected, StatusModelUnavailable, StatusInvalidRequest, StatusStreamUnsupported} {
		if !IsTerminal(status) {
			t.Errorf("expected %q terminal", status)
		}
	}
	for _, status := range []string{StatusReserved, StatusUpstreamPending, "settlement_pending", "running"} {
		if IsTerminal(status) {
			t.Errorf("expected %q non-terminal", status)
		}
	}
}
