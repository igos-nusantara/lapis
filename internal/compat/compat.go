// Package compat holds the Android-side contract of the LineageOS
// container images lapis boots: property names, binder service and
// interface names, window app ids and the image OTA channel. The images
// look these up by name, so they must match exactly; every other package
// refers to them only through this file.
package compat

import "strings"

// ns is the namespace the images use for their properties, services and
// AIDL interfaces.
const ns = "waydroid"

// Prop returns a namespaced property name, e.g. Prop("active_apps").
func Prop(name string) string { return ns + "." + name }

// PersistProp returns a namespaced persistent property name.
func PersistProp(name string) string { return "persist." + ns + "." + name }

// ProductProp returns the per-product override property for key.
func ProductProp(key string) string { return "ro.product." + ns + "." + key }

// PropFile is where the images read host-provided properties.
const PropFile = "/vendor/" + ns + ".prop"

// Service returns the binder service name for a host or image service.
func Service(name string) string { return ns + name }

// Interface returns the AIDL interface descriptor for name.
func Interface(name string) string { return "lineageos." + ns + "." + name }

// FullUIApp is the active-app value (and window app id) of the whole
// Android desktop shown in one window.
var FullUIApp = strings.ToUpper(ns[:1]) + ns[1:]

// AppID returns the Wayland app id of an Android app's window.
func AppID(pkg string) string { return ns + "." + pkg }

// ToolsVersion is the host-tools version the images expect.
const ToolsVersion = "1.6.3"

// Default OTA channels of the images.
const (
	SystemChannel = "https://ota.waydro.id/system"
	VendorChannel = "https://ota.waydro.id/vendor"
)

// DeviceDir returns the per-architecture directory name in a channel.
func DeviceDir(arch string) string { return ns + "_" + arch }
