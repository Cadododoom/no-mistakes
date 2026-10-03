package types

import "testing"

func TestMCPRequirementsRequireExactConfigurationKey(t *testing.T) {
 for _, name := range []string{"cloudflare", "Cloudflare", "CLOUDFLARE"} {
  _, parseErr := ParseMCPRequirement("review:"+name)
  _, canonicalErr := CanonicalMCPRequirements([]MCPRequirement{{Stage: StepTest, Server: name}})
  _, storedErr := ParseMCPRequirements(`[{"stage":"test","server":"`+name+`"}]`)
  for _, err := range []error{parseErr, canonicalErr, storedErr} {
   if (err == nil) != (name == "cloudflare") { t.Fatalf("key=%s error=%v", name, err) }
  }
 }
}
