package pairing

import (
	"encoding/json"
	"fmt"

	qrcode "github.com/skip2/go-qrcode"
)

// PairingInfo is encoded into the QR code for the mobile app to scan.
type PairingInfo struct {
	RelayURL string `json:"relay_url"`
	AgentID  string `json:"agent_id"`
	Secret   string `json:"secret"`
}

// GenerateQR creates a QR code string for terminal display.
func GenerateQR(relayURL, agentID, secret string) (string, error) {
	info := PairingInfo{
		RelayURL: relayURL,
		AgentID:  agentID,
		Secret:   secret,
	}

	data, err := json.Marshal(info)
	if err != nil {
		return "", fmt.Errorf("marshal pairing info: %w", err)
	}

	// Use the transitive:// scheme for deep linking.
	uri := fmt.Sprintf("transitive://%s", string(data))

	qr, err := qrcode.New(uri, qrcode.Medium)
	if err != nil {
		return "", fmt.Errorf("generate qr: %w", err)
	}

	return qr.ToSmallString(false), nil
}

// PrintQR displays the QR code in the terminal.
func PrintQR(relayURL, agentID, secret string) error {
	art, err := GenerateQR(relayURL, agentID, secret)
	if err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("Scan this QR code with the Transitive iOS app:")
	fmt.Println()
	fmt.Print(art)
	fmt.Println()
	fmt.Printf("Agent ID: %s\n", agentID)
	fmt.Printf("Relay:    %s\n", relayURL)
	fmt.Println()

	info := PairingInfo{RelayURL: relayURL, AgentID: agentID, Secret: secret}
	data, _ := json.Marshal(info)
	fmt.Printf("Pairing JSON (for manual entry):\n%s\n\n", string(data))

	return nil
}
