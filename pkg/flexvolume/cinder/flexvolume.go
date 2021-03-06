/*
Copyright 2017 The Kubernetes Authors.

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

package cinder

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/golang/glog"
	"k8s.io/frakti/pkg/flexvolume"
	utilmetadata "k8s.io/frakti/pkg/util/metadata"
)

type FlexVolumeDriver struct {
	uuid string
	name string

	volId        string
	fsType       string
	cinderConfig string
	readOnly     bool
	manager      *FlexManager

	// metadata provides meta of the volume
	metadata map[string]interface{}
}

// NewFlexVolumeDriver returns a flex volume driver
func NewFlexVolumeDriver(uuid string, name string) *FlexVolumeDriver {
	return &FlexVolumeDriver{
		uuid: uuid,
		name: name,
	}
}

// Invocation: <driver executable> init
func (d *FlexVolumeDriver) init() (map[string]interface{}, error) {
	// "{\"status\": \"Success\", \"capabilities\": {\"attach\": false}}"
	return map[string]interface{}{
		"capabilities": map[string]bool{
			"attach": false,
		},
	}, nil
}

// initFlexVolumeDriverForMount parse user provided jsonOptions to initialize FlexVolumeDriver
func (d *FlexVolumeDriver) initFlexVolumeDriverForMount(jsonOptions string) error {
	var volOptions map[string]interface{}
	json.Unmarshal([]byte(jsonOptions), &volOptions)

	if len(volOptions[flexvolume.VolIdKey].(string)) == 0 {
		return fmt.Errorf("jsonOptions is not set by user properly: %#v", jsonOptions)
	}

	// cinder configure file is optional in jsonOptions
	if userConfig, ok := volOptions[flexvolume.CinderConfigKey]; ok {
		d.cinderConfig = userConfig.(string)
	} else {
		// use default configure if not provided
		d.cinderConfig = flexvolume.CinderConfigFile
	}

	d.volId = volOptions[flexvolume.VolIdKey].(string)
	// this is a system option
	d.fsType = volOptions["kubernetes.io/fsType"].(string)

	manager, err := NewFlexManager(d.cinderConfig)
	if err != nil {
		return err
	}
	d.manager = manager

	return nil
}

// initFlexVolumeDriverForUnMount use targetMountDir to initialize FlexVolumeDriver from magic file
func (d *FlexVolumeDriver) initFlexVolumeDriverForUnMount(targetMountDir string) error {
	// use the magic file to store volId since flexvolume will execute fresh new binary every time
	var optsData flexvolume.FlexVolumeOptsData
	err := flexvolume.ReadJsonOptsFile(targetMountDir, &optsData)
	if err != nil {
		return err
	}

	d.cinderConfig = optsData.CinderData.ConfigKey

	d.volId = optsData.CinderData.VolumeID

	manager, err := NewFlexManager(d.cinderConfig)
	if err != nil {
		return err
	}
	d.manager = manager

	return nil
}

// Invocation: <driver executable> attach <json options> <node name>
func (d *FlexVolumeDriver) attach(jsonOptions, nodeName string) (map[string]interface{}, error) {
	return nil, nil
}

// Invocation: <driver executable> detach <mount device> <node name>
func (d *FlexVolumeDriver) detach(mountDev, nodeName string) (map[string]interface{}, error) {
	return nil, nil
}

// Invocation: <driver executable> waitforattach <mount device> <json options>
func (d *FlexVolumeDriver) waitForAttach(mountDev, jsonOptions string) (map[string]interface{}, error) {
	return map[string]interface{}{"device": mountDev}, nil
}

// Invocation: <driver executable> isattached <json options> <node name>
func (d *FlexVolumeDriver) isAttached(jsonOptions, nodeName string) (map[string]interface{}, error) {
	return map[string]interface{}{"attached": true}, nil
}

// Invocation: <driver executable> mount <mount dir> <json options>
// mount will:
// 1. attach Cinder volume to target dir by AttachDisk
// 2. store meta data generated by AttachDisk into a json file in target dir
func (d *FlexVolumeDriver) mount(targetMountDir, jsonOptions string) (map[string]interface{}, error) {
	glog.V(5).Infof("Cinder flexvolume mount %s to %s", d.volId, targetMountDir)

	// initialize cinder driver from user provided jsonOptions
	if err := d.initFlexVolumeDriverForMount(jsonOptions); err != nil {
		return nil, err
	}

	// attach cinder disk to host machine
	if err := d.manager.AttachDisk(d, targetMountDir); err != nil {
		glog.V(4).Infof("AttachDisk failed: %v", err)
		return nil, err
	}
	glog.V(3).Infof("Cinder volume %s attached", d.volId)

	// append VolumeOptions with metadata
	optsData := &flexvolume.FlexVolumeOptsData{
		CinderData: d.generateOptionsData(d.metadata),
	}
	// create a file and write metadata into the it
	if err := flexvolume.WriteJsonOptsFile(targetMountDir, optsData); err != nil {
		os.Remove(targetMountDir)
		detachDiskLogError(d)
		return nil, err
	}

	return nil, nil
}

func (d *FlexVolumeDriver) generateOptionsData(metadata map[string]interface{}) *flexvolume.CinderVolumeOptsData {
	var result *flexvolume.CinderVolumeOptsData

	result.ConfigKey = d.cinderConfig
	result.VolumeID = d.volId
	result.FsType = d.fsType

	if data, ok := metadata["volume_type"]; ok {
		result.VolumeType = data.(string)
	}
	if data, ok := metadata["name"]; ok {
		result.Name = data.(string)
	}

	if data, ok := metadata["hosts"]; ok {
		if hosts, err := utilmetadata.ExtractStringSlice(data); err != nil {
			glog.V(4).Infof("cannot parse metadata hosts: %v", err)
		} else {
			result.Hosts = hosts
		}
	}

	if data, ok := metadata["ports"]; ok {
		if ports, err := utilmetadata.ExtractStringSlice(data); err != nil {
			glog.V(4).Infof("cannot parse metadata ports: %v", err)
		} else {
			result.Ports = ports
		}
	}

	return result
}

// detachDiskLogError is a wrapper to detach first before log error
func detachDiskLogError(d *FlexVolumeDriver) {
	err := d.manager.DetachDisk(d)
	if err != nil {
		glog.Warningf("Failed to detach disk: %v (%v)", d, err)
	}
}

// Invocation: <driver executable> unmount <mount dir>
func (d *FlexVolumeDriver) unmount(targetMountDir string) (map[string]interface{}, error) {
	glog.V(5).Infof("Cinder flexvolume unmount of %s", targetMountDir)

	// check the target directory
	if _, err := os.Stat(targetMountDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("volume directory: %v does not exists", targetMountDir)
	}

	//  initialize FlexVolumeDriver manager by reading cinderConfig from metadata file
	if err := d.initFlexVolumeDriverForUnMount(targetMountDir); err != nil {
		return nil, err
	}

	if err := d.manager.DetachDisk(d); err != nil {
		return nil, err
	}

	// NOTE: the targetDir will be cleaned by flexvolume,
	// we just need to clean up the metadata file.
	if err := flexvolume.CleanUpMetadataFile(targetMountDir); err != nil {
		return nil, err
	}

	return nil, nil
}

type driverOp func(*FlexVolumeDriver, []string) (map[string]interface{}, error)

type cmdInfo struct {
	numArgs int
	run     driverOp
}

var commands = map[string]cmdInfo{
	"init": {
		0, func(d *FlexVolumeDriver, args []string) (map[string]interface{}, error) {
			return d.init()
		},
	},
	"attach": {
		2, func(d *FlexVolumeDriver, args []string) (map[string]interface{}, error) {
			return d.attach(args[0], args[1])
		},
	},
	"detach": {
		2, func(d *FlexVolumeDriver, args []string) (map[string]interface{}, error) {
			return d.detach(args[0], args[1])
		},
	},
	"waitforattach": {
		2, func(d *FlexVolumeDriver, args []string) (map[string]interface{}, error) {
			return d.waitForAttach(args[0], args[1])
		},
	},
	"isattached": {
		2, func(d *FlexVolumeDriver, args []string) (map[string]interface{}, error) {
			return d.isAttached(args[0], args[1])
		},
	},
	"mount": {
		2, func(d *FlexVolumeDriver, args []string) (map[string]interface{}, error) {
			return d.mount(args[0], args[1])
		},
	},
	"unmount": {
		1, func(d *FlexVolumeDriver, args []string) (map[string]interface{}, error) {
			return d.unmount(args[0])
		},
	},
}

func (d *FlexVolumeDriver) doRun(args []string) (map[string]interface{}, error) {
	if len(args) == 0 {
		return nil, errors.New("no arguments passed to flexvolume driver")
	}
	nArgs := len(args) - 1
	op := args[0]
	if cmdInfo, found := commands[op]; found {
		if cmdInfo.numArgs == nArgs {
			return cmdInfo.run(d, args[1:])
		} else {
			return nil, fmt.Errorf("unexpected number of args %d (expected %d) for operation %q", nArgs, cmdInfo.numArgs, op)
		}
	} else {
		return map[string]interface{}{
			"status": "Not supported",
		}, nil
	}
}

func (d *FlexVolumeDriver) Run(args []string) string {
	r := formatResult(d.doRun(args))

	return r
}

func formatResult(fields map[string]interface{}, err error) string {
	var data map[string]interface{}
	if err != nil {
		data = map[string]interface{}{
			"status":  "Failure",
			"message": err.Error(),
		}
	} else {
		data = map[string]interface{}{
			"status": "Success",
		}
		for k, v := range fields {
			data[k] = v
		}
	}
	s, err := json.Marshal(data)
	if err != nil {
		panic("error marshalling the data")
	}
	return string(s) + "\n"
}
