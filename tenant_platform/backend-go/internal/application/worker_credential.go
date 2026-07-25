package application

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeTokenOnlyMyKey writes a mykey.py containing ONLY the capability_token
// and Proxy URL — no real upstream key. The Worker's llmcore.py reads this
// config and calls the Proxy, which holds the real key (security red line:
// the platform ALWAYS overwrites this file; a user-provided mykey.py is
// ignored and replaced).
func writeTokenOnlyMyKey(configRoot, proxyAddr, token string) error {
	content := fmt.Sprintf(
		"native_oai_config = {\n"+
			"    'name': 'platform-capability-token',\n"+
			"    'apikey': %q,\n"+
			"    'apibase': %q,\n"+
			"    'model': 'gpt-test',\n"+
			"    'api_mode': 'chat_completions',\n"+
			"    'stream': False,\n"+
			"    'read_timeout': 30,\n"+
			"}\n",
		token, proxyAddr,
	)
	if err := os.MkdirAll(configRoot, 0o755); err != nil {
		return err
	}
	path := filepath.Join(configRoot, "mykey.py")
	// Overwrite unconditionally: the platform owns this file. The security
	// model forbids trusting a user-provided mykey.py (it might carry a real
	// key, violating spec §7.1).
	return os.WriteFile(path, []byte(content), 0o600)
}
