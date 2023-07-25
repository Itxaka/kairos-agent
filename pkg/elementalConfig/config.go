/*
Copyright © 2022 SUSE LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package elementalConfig

import (
	"fmt"
	"gopkg.in/yaml.v3"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"

	"github.com/kairos-io/kairos-agent/v2/internal/common"
	"github.com/kairos-io/kairos-agent/v2/pkg/cloudinit"
	agentConfig "github.com/kairos-io/kairos-agent/v2/pkg/config"
	"github.com/kairos-io/kairos-agent/v2/pkg/constants"
	"github.com/kairos-io/kairos-agent/v2/pkg/http"
	v1 "github.com/kairos-io/kairos-agent/v2/pkg/types/v1"
	"github.com/kairos-io/kairos-agent/v2/pkg/utils"
	"github.com/mitchellh/mapstructure"
	"github.com/sanity-io/litter"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"github.com/twpayne/go-vfs"
	"k8s.io/mount-utils"
)

type GenericOptions func(a *v1.Config)

func WithFs(fs v1.FS) func(r *v1.Config) {
	return func(r *v1.Config) {
		r.Fs = fs
	}
}

func WithLogger(logger v1.Logger) func(r *v1.Config) {
	return func(r *v1.Config) {
		r.Logger = logger
	}
}

func WithSyscall(syscall v1.SyscallInterface) func(r *v1.Config) {
	return func(r *v1.Config) {
		r.Syscall = syscall
	}
}

func WithMounter(mounter mount.Interface) func(r *v1.Config) {
	return func(r *v1.Config) {
		r.Mounter = mounter
	}
}

func WithRunner(runner v1.Runner) func(r *v1.Config) {
	return func(r *v1.Config) {
		r.Runner = runner
	}
}

func WithClient(client v1.HTTPClient) func(r *v1.Config) {
	return func(r *v1.Config) {
		r.Client = client
	}
}

func WithCloudInitRunner(ci v1.CloudInitRunner) func(r *v1.Config) {
	return func(r *v1.Config) {
		r.CloudInitRunner = ci
	}
}

func WithPlatform(platform string) func(r *v1.Config) {
	return func(r *v1.Config) {
		p, err := v1.ParsePlatform(platform)
		if err == nil {
			r.Platform = p
		}
	}
}

func WithImageExtractor(extractor v1.ImageExtractor) func(r *v1.Config) {
	return func(r *v1.Config) {
		r.ImageExtractor = extractor
	}
}

func NewConfig(opts ...GenericOptions) *v1.Config {
	log := v1.NewLogger()

	defaultPlatform, err := v1.NewPlatformFromArch(runtime.GOARCH)
	if err != nil {
		log.Errorf("error parsing default platform (%s): %s", runtime.GOARCH, err.Error())
		return nil
	}

	arch, err := utils.GolangArchToArch(runtime.GOARCH)
	if err != nil {
		log.Errorf("invalid arch: %s", err.Error())
		return nil
	}

	c := &v1.Config{
		Fs:                        vfs.OSFS,
		Logger:                    log,
		Syscall:                   &v1.RealSyscall{},
		Client:                    http.NewClient(),
		Arch:                      arch,
		Platform:                  defaultPlatform,
		SquashFsCompressionConfig: constants.GetDefaultSquashfsCompressionOptions(),
	}
	for _, o := range opts {
		o(c)
	}

	// delay runner creation after we have run over the options in case we use WithRunner
	if c.Runner == nil {
		c.Runner = &v1.RealRunner{Logger: c.Logger}
	}

	// Now check if the runner has a logger inside, otherwise point our logger into it
	// This can happen if we set the WithRunner option as that doesn't set a logger
	if c.Runner.GetLogger() == nil {
		c.Runner.SetLogger(c.Logger)
	}

	// Delay the yip runner creation, so we set the proper logger instead of blindly setting it to the logger we create
	// at the start of NewConfig, as WithLogger can be passed on init, and that would result in 2 different logger
	// instances, on the config.Logger and the other on config.CloudInitRunner
	if c.CloudInitRunner == nil {
		c.CloudInitRunner = cloudinit.NewYipCloudInitRunner(c.Logger, c.Runner, vfs.OSFS)
	}

	if c.Mounter == nil {
		c.Mounter = mount.New(constants.MountBinary)
	}

	return c
}

// NewInstallSpec returns an InstallSpec struct all based on defaults and basic host checks (e.g. EFI vs BIOS)
func NewInstallSpec(cfg *v1.Config) *v1.InstallSpec {
	var firmware string
	var recoveryImg, activeImg, passiveImg v1.Image

	recoveryImgFile := filepath.Join(constants.LiveDir, constants.RecoverySquashFile)

	// Check if current host has EFI firmware
	efiExists, _ := utils.Exists(cfg.Fs, constants.EfiDevice)
	// Check the default ISO installation media is available
	isoRootExists, _ := utils.Exists(cfg.Fs, constants.IsoBaseTree)
	// Check the default ISO recovery installation media is available)
	recoveryExists, _ := utils.Exists(cfg.Fs, recoveryImgFile)

	if efiExists {
		firmware = v1.EFI
	} else {
		firmware = v1.BIOS
	}

	activeImg.Label = constants.ActiveLabel
	activeImg.Size = constants.ImgSize
	activeImg.File = filepath.Join(constants.StateDir, "cOS", constants.ActiveImgFile)
	activeImg.FS = constants.LinuxImgFs
	activeImg.MountPoint = constants.ActiveDir
	if isoRootExists {
		activeImg.Source = v1.NewDirSrc(constants.IsoBaseTree)
	} else {
		activeImg.Source = v1.NewEmptySrc()
	}

	if recoveryExists {
		recoveryImg.Source = v1.NewFileSrc(recoveryImgFile)
		recoveryImg.FS = constants.SquashFs
		recoveryImg.File = filepath.Join(constants.RecoveryDir, "cOS", constants.RecoverySquashFile)
		recoveryImg.Size = constants.ImgSize
	} else {
		recoveryImg.Source = v1.NewFileSrc(activeImg.File)
		recoveryImg.FS = constants.LinuxImgFs
		recoveryImg.Label = constants.SystemLabel
		recoveryImg.File = filepath.Join(constants.RecoveryDir, "cOS", constants.RecoveryImgFile)
		recoveryImg.Size = constants.ImgSize
	}

	passiveImg = v1.Image{
		File:   filepath.Join(constants.StateDir, "cOS", constants.PassiveImgFile),
		Label:  constants.PassiveLabel,
		Source: v1.NewFileSrc(activeImg.File),
		FS:     constants.LinuxImgFs,
		Size:   constants.ImgSize,
	}

	return &v1.InstallSpec{
		Firmware:   firmware,
		PartTable:  v1.GPT,
		Partitions: NewInstallElementalParitions(),
		GrubConf:   constants.GrubConf,
		Tty:        constants.DefaultTty,
		Active:     activeImg,
		Recovery:   recoveryImg,
		Passive:    passiveImg,
	}
}

func NewInstallElementalParitions() v1.ElementalPartitions {
	partitions := v1.ElementalPartitions{}
	partitions.OEM = &v1.Partition{
		FilesystemLabel: constants.OEMLabel,
		Size:            constants.OEMSize,
		Name:            constants.OEMPartName,
		FS:              constants.LinuxFs,
		MountPoint:      constants.OEMDir,
		Flags:           []string{},
	}

	partitions.Recovery = &v1.Partition{
		FilesystemLabel: constants.RecoveryLabel,
		Size:            constants.RecoverySize,
		Name:            constants.RecoveryPartName,
		FS:              constants.LinuxFs,
		MountPoint:      constants.RecoveryDir,
		Flags:           []string{},
	}

	partitions.State = &v1.Partition{
		FilesystemLabel: constants.StateLabel,
		Size:            constants.StateSize,
		Name:            constants.StatePartName,
		FS:              constants.LinuxFs,
		MountPoint:      constants.StateDir,
		Flags:           []string{},
	}

	partitions.Persistent = &v1.Partition{
		FilesystemLabel: constants.PersistentLabel,
		Size:            constants.PersistentSize,
		Name:            constants.PersistentPartName,
		FS:              constants.LinuxFs,
		MountPoint:      constants.PersistentDir,
		Flags:           []string{},
	}
	return partitions
}

// NewUpgradeSpec returns an UpgradeSpec struct all based on defaults and current host state
func NewUpgradeSpec(cfg *v1.Config) (*v1.UpgradeSpec, error) {
	var recLabel, recFs, recMnt string
	var active, passive, recovery v1.Image

	installState, err := cfg.LoadInstallState()
	if err != nil {
		cfg.Logger.Warnf("failed reading installation state: %s", err.Error())
	}

	parts, err := utils.GetAllPartitions()
	if err != nil {
		return nil, fmt.Errorf("could not read host partitions")
	}
	ep := v1.NewElementalPartitionsFromList(parts)

	if ep.Recovery == nil {
		// We could have recovery in lvm which won't appear in ghw list
		ep.Recovery = utils.GetPartitionViaDM(cfg.Fs, constants.RecoveryLabel)
	}

	if ep.OEM == nil {
		// We could have OEM in lvm which won't appear in ghw list
		ep.OEM = utils.GetPartitionViaDM(cfg.Fs, constants.OEMLabel)
	}

	if ep.Persistent == nil {
		// We could have persistent encrypted or in lvm which won't appear in ghw list
		ep.Persistent = utils.GetPartitionViaDM(cfg.Fs, constants.PersistentLabel)
	}

	if ep.Recovery != nil {
		if ep.Recovery.MountPoint == "" {
			ep.Recovery.MountPoint = constants.RecoveryDir
		}

		squashedRec, err := utils.HasSquashedRecovery(cfg, ep.Recovery)
		if err != nil {
			return nil, fmt.Errorf("failed checking for squashed recovery")
		}

		if squashedRec {
			recFs = constants.SquashFs
		} else {
			recLabel = constants.SystemLabel
			recFs = constants.LinuxImgFs
			recMnt = constants.TransitionDir
		}

		recovery = v1.Image{
			File:       filepath.Join(ep.Recovery.MountPoint, "cOS", constants.TransitionImgFile),
			Size:       constants.ImgSize,
			Label:      recLabel,
			FS:         recFs,
			MountPoint: recMnt,
			Source:     v1.NewEmptySrc(),
		}
	}

	if ep.State != nil {
		if ep.State.MountPoint == "" {
			ep.State.MountPoint = constants.StateDir
		}

		active = v1.Image{
			File:       filepath.Join(ep.State.MountPoint, "cOS", constants.TransitionImgFile),
			Size:       constants.ImgSize,
			Label:      constants.ActiveLabel,
			FS:         constants.LinuxImgFs,
			MountPoint: constants.TransitionDir,
			Source:     v1.NewEmptySrc(),
		}

		passive = v1.Image{
			File:   filepath.Join(ep.State.MountPoint, "cOS", constants.PassiveImgFile),
			Label:  constants.PassiveLabel,
			Size:   constants.ImgSize,
			Source: v1.NewFileSrc(active.File),
			FS:     active.FS,
		}
	}

	// If we have oem in the system, but it has no mountpoint
	if ep.OEM != nil && ep.OEM.MountPoint == "" {
		// Add the default mountpoint for it in case the chroot stages want to bind mount it
		ep.OEM.MountPoint = constants.OEMPath
	}
	// This is needed if we want to use the persistent as tmpdir for the upgrade images
	// as tmpfs is 25% of the total RAM, we cannot rely on the tmp dir having enough space for our image
	// This enables upgrades on low ram devices
	if ep.Persistent != nil {
		if ep.Persistent.MountPoint == "" {
			ep.Persistent.MountPoint = constants.PersistentDir
		}
	}

	return &v1.UpgradeSpec{
		Active:     active,
		Recovery:   recovery,
		Passive:    passive,
		Partitions: ep,
		State:      installState,
	}, nil
}

// NewResetSpec returns a ResetSpec struct all based on defaults and current host state
func NewResetSpec(cfg *v1.Config) (*v1.ResetSpec, error) {
	var imgSource *v1.ImageSource

	//TODO find a way to pre-load current state values such as labels
	if !utils.BootedFrom(cfg.Runner, constants.RecoverySquashFile) &&
		!utils.BootedFrom(cfg.Runner, constants.SystemLabel) {
		return nil, fmt.Errorf("reset can only be called from the recovery system")
	}

	efiExists, _ := utils.Exists(cfg.Fs, constants.EfiDevice)

	installState, err := cfg.LoadInstallState()
	if err != nil {
		cfg.Logger.Warnf("failed reading installation state: %s", err.Error())
	}

	parts, err := utils.GetAllPartitions()
	if err != nil {
		return nil, fmt.Errorf("could not read host partitions")
	}
	ep := v1.NewElementalPartitionsFromList(parts)

	if efiExists {
		if ep.EFI == nil {
			return nil, fmt.Errorf("EFI partition not found")
		}
		if ep.EFI.MountPoint == "" {
			ep.EFI.MountPoint = constants.EfiDir
		}
		ep.EFI.Name = constants.EfiPartName
	}

	if ep.State == nil {
		return nil, fmt.Errorf("state partition not found")
	}
	if ep.State.MountPoint == "" {
		ep.State.MountPoint = constants.StateDir
	}
	ep.State.Name = constants.StatePartName

	if ep.Recovery == nil {
		// We could have recovery in lvm which won't appear in ghw list
		ep.Recovery = utils.GetPartitionViaDM(cfg.Fs, constants.RecoveryLabel)
		if ep.Recovery == nil {
			return nil, fmt.Errorf("recovery partition not found")
		}
	}
	if ep.Recovery.MountPoint == "" {
		ep.Recovery.MountPoint = constants.RecoveryDir
	}

	target := ep.State.Disk

	// OEM partition is not a hard requirement
	if ep.OEM != nil {
		if ep.OEM.MountPoint == "" {
			ep.OEM.MountPoint = constants.OEMDir
		}
		ep.OEM.Name = constants.OEMPartName
	} else {
		// We could have oem in lvm which won't appear in ghw list
		ep.OEM = utils.GetPartitionViaDM(cfg.Fs, constants.OEMLabel)
	}

	if ep.OEM == nil {
		cfg.Logger.Warnf("no OEM partition found")
	}

	// Persistent partition is not a hard requirement
	if ep.Persistent != nil {
		if ep.Persistent.MountPoint == "" {
			ep.Persistent.MountPoint = constants.PersistentDir
		}
		ep.Persistent.Name = constants.PersistentPartName
	} else {
		// We could have persistent encrypted or in lvm which won't appear in ghw list
		ep.Persistent = utils.GetPartitionViaDM(cfg.Fs, constants.PersistentLabel)
	}
	if ep.Persistent == nil {
		cfg.Logger.Warnf("no Persistent partition found")
	}

	recoveryImg := filepath.Join(constants.RunningStateDir, "cOS", constants.RecoveryImgFile)
	recoveryImg2 := filepath.Join(constants.RunningRecoveryStateDir, "cOS", constants.RecoveryImgFile)

	if exists, _ := utils.Exists(cfg.Fs, recoveryImg); exists {
		imgSource = v1.NewFileSrc(recoveryImg)
	} else if exists, _ = utils.Exists(cfg.Fs, recoveryImg2); exists {
		imgSource = v1.NewFileSrc(recoveryImg2)
	} else if exists, _ = utils.Exists(cfg.Fs, constants.IsoBaseTree); exists {
		imgSource = v1.NewDirSrc(constants.IsoBaseTree)
	} else {
		imgSource = v1.NewEmptySrc()
	}

	activeFile := filepath.Join(ep.State.MountPoint, "cOS", constants.ActiveImgFile)
	return &v1.ResetSpec{
		Target:           target,
		Partitions:       ep,
		Efi:              efiExists,
		GrubDefEntry:     constants.GrubDefEntry,
		GrubConf:         constants.GrubConf,
		Tty:              constants.DefaultTty,
		FormatPersistent: true,
		Active: v1.Image{
			Label:      constants.ActiveLabel,
			Size:       constants.ImgSize,
			File:       activeFile,
			FS:         constants.LinuxImgFs,
			Source:     imgSource,
			MountPoint: constants.ActiveDir,
		},
		Passive: v1.Image{
			File:   filepath.Join(ep.State.MountPoint, "cOS", constants.PassiveImgFile),
			Label:  constants.PassiveLabel,
			Size:   constants.ImgSize,
			Source: v1.NewFileSrc(activeFile),
			FS:     constants.LinuxImgFs,
		},
		State: installState,
	}, nil
}

// ReadConfigRunFromAgentConfig reads the configuration directly from a given cloud config string
func ReadConfigRunFromAgentConfig(c *agentConfig.Config) (*v1.Config, error) {
	cfg := NewConfig(WithLogger(v1.NewLogger()), WithImageExtractor(v1.OCIImageExtractor{}))
	var err error

	ccString, err := c.Config.String()
	if err != nil {
		return nil, err
	}

	// Load any cloud-config values that override our default Config
	err = yaml.Unmarshal([]byte(ccString), &cfg)
	if err != nil {
		return nil, err
	}
	// If we got debug enabled via cloud config, set it on viper so its available everywhere
	if cfg.Debug {
		viper.Set("debug", true)
	}
	configLogger(cfg.Logger, cfg.Fs)
	// Store the full cloud-config in here, so we can reuse it afterward for the spec
	cfg.FullCloudConfig = ccString
	err = cfg.Sanitize()
	cfg.Logger.Debugf("Full config loaded: %s", litter.Sdump(cfg))
	return cfg, err
}

// ReadSpecFromCloudConfig returns a v1.Spec for the given spec
func ReadSpecFromCloudConfig(r *v1.Config, spec string) (v1.Spec, error) {
	var sp v1.Spec
	var err error

	switch spec {
	case "install":
		sp = NewInstallSpec(r)
	case "upgrade":
		sp, err = NewUpgradeSpec(r)
	case "reset":
		sp, err = NewResetSpec(r)
	default:
		return nil, fmt.Errorf("spec not valid: %s", spec)
	}
	if err != nil {
		return nil, fmt.Errorf("failed initializing spec: %v", err)
	}

	// Load the config into viper from the raw cloud config string
	viper.SetConfigType("yaml")
	viper.ReadConfig(strings.NewReader(r.FullCloudConfig))
	vp := viper.Sub(spec)
	if vp == nil {
		vp = viper.New()
	}

	err = vp.Unmarshal(sp, setDecoder, decodeHook)
	if err != nil {
		r.Logger.Warnf("error unmarshalling %s Spec: %s", spec, err)
	}
	r.Logger.Debugf("Loaded %s spec: %s", litter.Sdump(sp))
	return sp, err
}

// readConfigAndSpecFromAgentConfig will return the config and spec for the given action based off the agent Config
func readConfigAndSpecFromAgentConfig(c *agentConfig.Config, action string) (*v1.Config, v1.Spec, error) {
	config, err := ReadConfigRunFromAgentConfig(c)
	if err != nil {
		return nil, nil, err
	}
	spec, err := ReadSpecFromCloudConfig(config, action)
	if err != nil {
		return nil, nil, err
	}
	return config, spec, nil
}

// ReadResetConfigFromAgentConfig will return a proper v1.Config and v1.ResetSpec based on an agent Config
func ReadResetConfigFromAgentConfig(c *agentConfig.Config) (*v1.Config, *v1.ResetSpec, error) {
	config, spec, err := readConfigAndSpecFromAgentConfig(c, "reset")
	if err != nil {
		return nil, nil, err
	}
	resetSpec := spec.(*v1.ResetSpec)
	return config, resetSpec, nil
}

func ReadInstallConfigFromAgentConfig(c *agentConfig.Config) (*v1.Config, *v1.InstallSpec, error) {
	config, spec, err := readConfigAndSpecFromAgentConfig(c, "install")
	if err != nil {
		return nil, nil, err
	}
	installSpec := spec.(*v1.InstallSpec)
	return config, installSpec, nil
}

func ReadUpgradeConfigFromAgentConfig(c *agentConfig.Config) (*v1.Config, *v1.UpgradeSpec, error) {
	config, spec, err := readConfigAndSpecFromAgentConfig(c, "upgrade")
	if err != nil {
		return nil, nil, err
	}
	upgradeSpec := spec.(*v1.UpgradeSpec)
	return config, upgradeSpec, nil
}

func configLogger(log v1.Logger, vfs v1.FS) {
	// Set debug level
	if viper.GetBool("debug") {
		log.SetLevel(v1.DebugLevel())
	}

	// Set formatter so both file and stdout format are equal
	log.SetFormatter(&logrus.TextFormatter{
		ForceColors:      true,
		DisableColors:    false,
		DisableTimestamp: false,
		FullTimestamp:    true,
	})

	// Logfile
	// Not being used for now, disable it until we plug it again in our cli
	/*
		logfile := viper.GetString("logfile")
		if logfile != "" {
			o, err := vfs.OpenFile(logfile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fs.ModePerm)

			if err != nil {
				log.Errorf("Could not open %s for logging to file: %s", logfile, err.Error())
			}

			if viper.GetBool("quiet") { // if quiet is set, only set the log to the file
				log.SetOutput(o)
			} else { // else set it to both stdout and the file
				mw := io.MultiWriter(os.Stdout, o)
				log.SetOutput(mw)
			}
		} else { // no logfile
			if viper.GetBool("quiet") { // quiet is enabled so discard all logging
				log.SetOutput(io.Discard)
			} else { // default to stdout
				log.SetOutput(os.Stdout)
			}
		}
	*/

	v := common.GetVersion()
	log.Infof("kairos-agent version %s", v)
}

var decodeHook = viper.DecodeHook(
	mapstructure.ComposeDecodeHookFunc(
		UnmarshalerHook(),
		mapstructure.StringToTimeDurationHookFunc(),
		mapstructure.StringToSliceHookFunc(","),
	),
)

type Unmarshaler interface {
	CustomUnmarshal(interface{}) (bool, error)
}

func UnmarshalerHook() mapstructure.DecodeHookFunc {
	return func(from reflect.Value, to reflect.Value) (interface{}, error) {
		// get the destination object address if it is not passed by reference
		if to.CanAddr() {
			to = to.Addr()
		}
		// If the destination implements the unmarshaling interface
		u, ok := to.Interface().(Unmarshaler)
		if !ok {
			return from.Interface(), nil
		}
		// If it is nil and a pointer, create and assign the target value first
		if to.IsNil() && to.Type().Kind() == reflect.Ptr {
			to.Set(reflect.New(to.Type().Elem()))
			u = to.Interface().(Unmarshaler)
		}
		// Call the custom unmarshaling method
		cont, err := u.CustomUnmarshal(from.Interface())
		if cont {
			// Continue with the decoding stack
			return from.Interface(), err
		}
		// Decoding finalized
		return to.Interface(), err
	}
}

func setDecoder(config *mapstructure.DecoderConfig) {
	// Make sure we zero fields before applying them, this is relevant for slices
	// so we do not merge with any already present value and directly apply whatever
	// we got form configs.
	config.ZeroFields = true
}
