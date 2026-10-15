package scaleway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/scaleway/packer-plugin-scaleway/internal/httperrors"
	block "github.com/scaleway/scaleway-sdk-go/api/block/v1alpha1"
	"github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"
)

type stepCreateServer struct {
	serverID string
}

func (s *stepCreateServer) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	instanceAPI := instance.NewAPI(state.Get("client").(*scw.Client))
	ui := state.Get("ui").(packersdk.Ui)
	c := state.Get("config").(*Config)

	// Add specific server_tags if provided, else apply the tags
	tags := c.ServerTags
	if len(tags) == 0 {
		tags = c.Tags
	}

	ui.Say("Creating server...")

	if c.Comm.SSHPublicKey != nil {
		tags = append(tags, "AUTHORIZED_KEY="+strings.ReplaceAll(strings.TrimSpace(string(c.Comm.SSHPublicKey)), " ", "_"))
	}

	bootType := instance.BootType(c.BootType)

	createServerReq := &instance.CreateServerRequest{
		BootType:       &bootType,
		CommercialType: c.CommercialType,
		Name:           c.ServerName,
		Image:          scw.StringPtr(c.Image),
		Tags:           tags,
		Zone:           scw.Zone(c.Zone),
	}

	if c.RootVolume.IsConfigured() {
		if createServerReq.Volumes == nil {
			createServerReq.Volumes = make(map[string]*instance.VolumeServerTemplate)
		}

		createServerReq.Volumes["0"] = c.RootVolume.VolumeServerTemplate()
	}

	if len(c.BlockVolumes) > 0 {
		if createServerReq.Volumes == nil {
			createServerReq.Volumes = make(map[string]*instance.VolumeServerTemplate)
		}

		createdVolumes := state.Get(StateKeyCreatedVolumes).([]*instance.VolumeServerTemplate)
		for i, blockVolume := range createdVolumes {
			volumeIndex := strconv.FormatInt(int64(i+1), 10)
			createServerReq.Volumes[volumeIndex] = blockVolume
		}
	}

	if c.AdminPasswordEncryptionSSHKeyID != "" {
		createServerReq.AdminPasswordEncryptionSSHKeyID = &c.AdminPasswordEncryptionSSHKeyID
	}

	createServerResp, err := createServer(instanceAPI, createServerReq, scw.WithContext(ctx))
	if err != nil {
		return putErrorAndHalt(state, ui, fmt.Errorf("error creating server: %w", formatInstanceError(err)))
	}

	// Track the server as soon as it exists so that every later halt
	// destroys it in Cleanup.
	s.serverID = createServerResp.Server.ID

	err = c.RootVolume.PostServerCreationSetup(block.NewAPI(state.Get("client").(*scw.Client)), createServerResp.Server)
	if err != nil {
		return putErrorAndHalt(state, ui, fmt.Errorf("error during post server creation setup server: %w", err))
	}

	if len(c.UserData) > 0 {
		m := map[string]io.Reader{}
		for k, v := range c.UserData {
			m[k] = bytes.NewBufferString(v)
		}

		createUserDataReq := &instance.SetAllServerUserDataRequest{
			Zone:     scw.Zone(c.Zone),
			ServerID: createServerResp.Server.ID,
			UserData: m,
		}

		err = instanceAPI.SetAllServerUserData(createUserDataReq, scw.WithContext(ctx))
		if err != nil {
			return putErrorAndHalt(state, ui, fmt.Errorf("error creating server: %w", err))
		}
	}

	server, err := startServer(ctx, instanceAPI, c, createServerResp.Server.ID)
	if err == nil && server.State == instance.ServerStateStopped {
		// The poweron was accepted, then failed asynchronously without
		// recording a reason on the server. Out of stock is the usual cause,
		// and a new poweron reports it synchronously, or starts the server if
		// capacity came back.
		ui.Say("Server stopped right after poweron, retrying once...")

		server, err = startServer(ctx, instanceAPI, c, createServerResp.Server.ID)
	}

	if err != nil {
		return putErrorAndHalt(state, ui, err)
	}

	if server.State != instance.ServerStateRunning {
		return putErrorAndHalt(state, ui, fmt.Errorf("server %s is %s instead of running", server.ID, server.State))
	}

	state.Put("server_id", createServerResp.Server.ID)
	// instance_id is the generic term used so that users can have access to the
	// instance id inside the provisioners, used in step_provision.
	state.Put("instance_id", s.serverID)

	return multistep.ActionContinue
}

func (s *stepCreateServer) Cleanup(state multistep.StateBag) {
	if s.serverID == "" || state.Get("config").(*Config).KeepServer {
		return
	}

	client := state.Get("client").(*scw.Client)
	instanceAPI := instance.NewAPI(client)
	ui := state.Get("ui").(packersdk.Ui)
	c := state.Get("config").(*Config)
	zone := scw.Zone(c.Zone)

	// Once the image exists, stepCreateVolume.Cleanup removes every server
	// volume. Before that, the root volume was created with the server and
	// nothing else removes it.
	var rootVolume *instance.VolumeServer

	if _, imageCreated := state.GetOk("snapshots"); !imageCreated && c.RemoveVolume {
		resp, err := instanceAPI.GetServer(&instance.GetServerRequest{
			ServerID: s.serverID,
			Zone:     zone,
		})
		if err != nil && !httperrors.Is404(err) {
			ui.Error(fmt.Sprintf("Error fetching server %s, its root volume may need manual removal: %s", s.serverID, err))
		}

		if err == nil {
			rootVolume = resp.Server.Volumes["0"]
		}
	}

	ui.Say("Destroying server...")

	deleteErr := instanceAPI.DeleteServer(&instance.DeleteServerRequest{
		ServerID: s.serverID,
		Zone:     zone,
	})
	if deleteErr != nil {
		_, err := instanceAPI.ServerAction(&instance.ServerActionRequest{
			Action:   instance.ServerActionTerminate,
			ServerID: s.serverID,
			Zone:     zone,
		})
		if err != nil {
			ui.Error(fmt.Sprintf("Error deleting server: %s", deleteErr))
			ui.Error(fmt.Sprintf(
				"Error terminating server. Please destroy it manually: %s", err))

			return
		}
	}

	if rootVolume == nil {
		return
	}

	ui.Say("Removing root volume...")

	var err error
	if rootVolume.VolumeType == instance.VolumeServerVolumeTypeSbsVolume {
		// Waits for the volume to be detached from the deleted server.
		err = waitAndDeleteBlockVolume(block.NewAPI(client), zone, rootVolume.ID)
	} else {
		err = waitAndDeleteInstanceVolume(instanceAPI, zone, rootVolume.ID)
	}

	if err != nil && !httperrors.Is404(err) {
		ui.Error(fmt.Sprintf("Error removing root volume %s. Please remove it manually: %s", rootVolume.ID, err))
	}
}

// startServer powers the server on and waits for it to settle in a terminal
// state, which can be stopped if the start failed.
func startServer(ctx context.Context, instanceAPI *instance.API, c *Config, serverID string) (*instance.Server, error) {
	_, err := instanceAPI.ServerAction(&instance.ServerActionRequest{
		Action:   instance.ServerActionPoweron,
		ServerID: serverID,
		Zone:     scw.Zone(c.Zone),
	}, scw.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("error starting server: %w", err)
	}

	server, err := instanceAPI.WaitForServer(&instance.WaitForServerRequest{
		ServerID: serverID,
		Zone:     scw.Zone(c.Zone),
		Timeout:  &c.ServerCreationTimeout,
	}, scw.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("server is not available: %w", err)
	}

	return server, nil
}
