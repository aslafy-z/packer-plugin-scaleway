package tests_test

import (
	"testing"

	"github.com/scaleway/packer-plugin-scaleway/internal/checks"
	"github.com/scaleway/packer-plugin-scaleway/internal/tester"
	"github.com/scaleway/scaleway-sdk-go/scw"
)

func TestFailedBuildCleanup(t *testing.T) {
	zone := scw.ZoneFrPar1

	tester.TestWithFailure(t, &tester.TestConfig{
		Config: `
source "scaleway" "failed" {
  commercial_type = "PRO2-XXS"
  zone = "fr-par-1"
  image = "ubuntu_jammy"
  image_name = "packer-e2e-failed-cleanup"
  ssh_username = "root"
  remove_volume = true
}

build {
  sources = ["source.scaleway.failed"]
  provisioner "shell" {
    inline = ["exit 1"]
  }
}
`,
		Checks: []tester.PackerCheck{
			checks.NoImage(zone, "packer-e2e-failed-cleanup"),
			checks.NoVolume(zone),
		},
	})
}
