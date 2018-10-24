package classic

import (
	"fmt"
	"log"
	"os"

	"github.com/hashicorp/go-cleanhttp"
	"github.com/hashicorp/go-oracle-terraform/compute"
	"github.com/hashicorp/go-oracle-terraform/opc"
	ocommon "github.com/hashicorp/packer/builder/oracle/common"
	"github.com/hashicorp/packer/common"
	"github.com/hashicorp/packer/helper/communicator"
	"github.com/hashicorp/packer/helper/multistep"
	"github.com/hashicorp/packer/packer"
)

// BuilderId uniquely identifies the builder
const BuilderId = "packer.oracle.classic"

// Builder is a builder implementation that creates Oracle OCI custom images.
type Builder struct {
	config *Config
	runner multistep.Runner
}

func (b *Builder) Prepare(rawConfig ...interface{}) ([]string, error) {
	config, err := NewConfig(rawConfig...)
	if err != nil {
		return nil, err
	}
	b.config = config

	var errs *packer.MultiError

	errs = packer.MultiErrorAppend(errs, b.config.PVConfig.Prepare(&b.config.ctx))

	if errs != nil && len(errs.Errors) > 0 {
		return nil, errs
	}
	return nil, nil
}

func (b *Builder) Run(ui packer.Ui, hook packer.Hook, cache packer.Cache) (packer.Artifact, error) {
	loggingEnabled := os.Getenv("PACKER_OCI_CLASSIC_LOGGING") != ""
	httpClient := cleanhttp.DefaultClient()
	config := &opc.Config{
		Username:       opc.String(b.config.Username),
		Password:       opc.String(b.config.Password),
		IdentityDomain: opc.String(b.config.IdentityDomain),
		APIEndpoint:    b.config.apiEndpointURL,
		LogLevel:       opc.LogDebug,
		Logger:         &Logger{loggingEnabled},
		// Logger: # Leave blank to use the default logger, or provide your own
		HTTPClient: httpClient,
	}
	// Create the Compute Client
	client, err := compute.NewComputeClient(config)
	if err != nil {
		return nil, fmt.Errorf("Error creating OPC Compute Client: %s", err)
	}

	runID := os.Getenv("PACKER_RUN_UUID")
	// Populate the state bag
	state := new(multistep.BasicStateBag)
	state.Put("config", b.config)
	state.Put("hook", hook)
	state.Put("ui", ui)
	state.Put("client", client)
	state.Put("run_id", runID)

	var steps []multistep.Step
	if b.config.IsPV() {
		builderCommConfig := b.config.Comm
		builderCommConfig.SSHPty = true

		steps = []multistep.Step{
			&stepCreatePersistentVolume{
				VolumeSize:     fmt.Sprintf("%d", b.config.PersistentVolumeSize),
				VolumeName:     fmt.Sprintf("master-storage_%s", runID),
				ImageList:      b.config.SourceImageList,
				ImageListEntry: b.config.SourceImageListEntry,
				Bootable:       true,
			},
			&stepCreatePersistentVolume{
				// We double the master volume size because we need room to
				// tarball the disk image. We also need to chunk the tar ball,
				// but we can remove the original disk image first.
				VolumeSize: fmt.Sprintf("%d", b.config.PersistentVolumeSize*2),
				VolumeName: fmt.Sprintf("builder-storage_%s", runID),
			},
			&ocommon.StepKeyPair{
				Debug:        b.config.PackerDebug,
				Comm:         &b.config.Comm,
				DebugKeyPath: fmt.Sprintf("oci_classic_%s.pem", b.config.PackerBuildName),
			},
			&stepCreateIPReservation{},
			&stepAddKeysToAPI{},
			&stepSecurity{},
			&stepCreatePVMaster{
				Name:       fmt.Sprintf("master-instance_%s", runID),
				VolumeName: fmt.Sprintf("master-storage_%s", runID),
			},
			&communicator.StepConnect{
				Config:    &b.config.Comm,
				Host:      ocommon.CommHost,
				SSHConfig: b.config.Comm.SSHConfigFunc(),
			},
			&common.StepProvision{},
			&stepTerminatePVMaster{},
			&stepCreatePVBuilder{
				Name:              fmt.Sprintf("builder-instance_%s", runID),
				BuilderVolumeName: fmt.Sprintf("builder-storage_%s", runID),
			},
			&stepAttachVolume{
				VolumeName:      fmt.Sprintf("master-storage_%s", runID),
				Index:           2,
				InstanceInfoKey: "builder_instance_info",
			},
			&communicator.StepConnect{
				Config:    &builderCommConfig,
				Host:      ocommon.CommHost,
				SSHConfig: b.config.Comm.SSHConfigFunc(),
			},
			&stepUploadImage{
				UploadImageCommand: b.config.BuilderUploadImageCommand,
			},
			&stepCreateImage{},
			&stepListImages{},
			&common.StepCleanupTempKeys{
				Comm: &b.config.Comm,
			},
		}
	} else {
		// Build the steps
		steps = []multistep.Step{
			&ocommon.StepKeyPair{
				Debug:        b.config.PackerDebug,
				Comm:         &b.config.Comm,
				DebugKeyPath: fmt.Sprintf("oci_classic_%s.pem", b.config.PackerBuildName),
			},
			&stepCreateIPReservation{},
			&stepAddKeysToAPI{},
			&stepSecurity{},
			&stepCreateInstance{},
			&communicator.StepConnect{
				Config:    &b.config.Comm,
				Host:      ocommon.CommHost,
				SSHConfig: b.config.Comm.SSHConfigFunc(),
			},
			&common.StepProvision{},
			&common.StepCleanupTempKeys{
				Comm: &b.config.Comm,
			},
			&stepSnapshot{},
			&stepListImages{},
		}
	}

	// Run the steps
	b.runner = common.NewRunner(steps, b.config.PackerConfig, ui)
	b.runner.Run(state)

	// If there was an error, return that
	if rawErr, ok := state.GetOk("error"); ok {
		return nil, rawErr.(error)
	}

	// If there is no snapshot, then just return
	if _, ok := state.GetOk("machine_image"); !ok {
		return nil, nil
	}

	// Build the artifact and return it
	artifact := &Artifact{
		ImageListVersion: state.Get("image_list_version").(int),
		MachineImageName: state.Get("machine_image_name").(string),
		MachineImageFile: state.Get("machine_image_file").(string),
	}

	return artifact, nil
}

// Cancel terminates a running build.
func (b *Builder) Cancel() {
	if b.runner != nil {
		log.Println("Cancelling the step runner...")
		b.runner.Cancel()
	}
}
