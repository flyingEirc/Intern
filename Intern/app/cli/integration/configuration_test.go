//go:build integration

package integration

import (
	"encoding/json/v2"

	"github.com/flyingEirc/IotProject/app/cli/internal/testenv"
)

func configuration(c *testenv.Case) {
	seedBasic(c)
	c.ConfigurationChecks()
	published := c.List()
	c.Assert("only-valid-configurations-published", len(published) == 2)
	for _, result := range published {
		var envelope struct {
			AAD []byte `json:"aad"`
		}
		c.Must(json.Unmarshal(c.Read("/work/backups/"+result.ID+"/envelope.json"), &envelope))
		var codec struct {
			Compression string `json:"compression"`
			Cipher      string `json:"cipher"`
		}
		c.Must(json.Unmarshal(envelope.AAD, &codec))
		c.Assert("explicit-false-codecs", codec.Compression == "none" && codec.Cipher == "none")
	}
	c.Root("mv", "/run/secrets/key", "/run/secrets/key-held")
	c.Restore(published[0].ID, true)
	c.Assert("unencrypted-restores-without-key", rows(c, c.Target, "restored") == "1:baseline|2:old|3:delete")
}
