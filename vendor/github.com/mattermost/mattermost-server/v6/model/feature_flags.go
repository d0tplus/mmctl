// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package model

import (
	"reflect"
	"strconv"
)

type FeatureFlags struct {
	// Exists only for unit and manual testing.
	// When set to a value, will be returned by the ping endpoint.
	TestFeature string
	// Exists only for testing bool functionality. Boolean feature flags interpret "on" or "true" as true and
	// all other values as false.
	TestBoolFeature bool

	// Toggle on and off scheduled jobs for cloud user limit emails see MM-29999
	CloudDelinquentEmailJobsEnabled bool

	// Toggle on and off support for Collapsed Threads
	CollapsedThreads bool

	// Enable the remote cluster service for shared channels.
	EnableRemoteClusterService bool

	// AppsEnabled toggles the Apps framework functionalities both in server and client side
	AppsEnabled bool

	// AppBarEnabled toggles the App Bar component on client side
	AppBarEnabled bool

	// Feature flags to control plugin versions
	PluginPlaybooks  string `plugin_id:"playbooks"`
	PluginApps       string `plugin_id:"com.mattermost.apps"`
	PluginFocalboard string `plugin_id:"focalboard"`

	PermalinkPreviews bool

	// Enable the Global Header
	GlobalHeader bool

	// Enable different team menu button treatments, possible values = ("none", "by_team_name", "inverted_sidebar_bg_color")
	AddChannelButton string

	// Determine whether when a user gets created, they'll have noisy notifications e.g. Send desktop notifications for all activity
	NewAccountNoisy bool

	// Enable Boards Unfurl Preview
	BoardsUnfurl bool

	// Enable Calls plugin support in the mobile app
	CallsMobile bool

	// Start A/B tour tips automatically, possible values = ("none", "auto")
	AutoTour string

	// A dash separated list for feature flags to turn on for Boards
	BoardsFeatureFlags string

	// A/B test for the add members to channel button, possible values = ("top", "bottom")
	AddMembersToChannel string

	// Enable Create First Channel
	GuidedChannelCreation bool

	// Determine after which duration in hours to send a second invitation to someone that didn't join after the initial invite, possible values = ("48", "72")
	ResendInviteEmailInterval string

	// A/B test for whether radio buttons or toggle button is more effective in in-screen invite to team modal ("none", "toggle")
	InviteToTeam string

	// Enable inline post editing
	InlinePostEditing bool
}

func (f *FeatureFlags) SetDefaults() {
	f.TestFeature = "off"
	f.TestBoolFeature = false
	f.CloudDelinquentEmailJobsEnabled = false
	f.CollapsedThreads = true
	f.EnableRemoteClusterService = false
	f.AppsEnabled = false
	f.AppBarEnabled = false
	f.PluginApps = ""
	f.PluginFocalboard = ""
	f.PermalinkPreviews = true
	f.GlobalHeader = true
	f.AddChannelButton = "by_team_name"
	f.NewAccountNoisy = false
	f.BoardsUnfurl = true
	f.CallsMobile = false
	f.AutoTour = "none"
	f.BoardsFeatureFlags = ""
	f.AddMembersToChannel = "top"
	f.GuidedChannelCreation = false
	f.ResendInviteEmailInterval = ""
	f.InviteToTeam = "none"
	f.InlinePostEditing = false
}

func (f *FeatureFlags) Plugins() map[string]string {
	rFFVal := reflect.ValueOf(f).Elem()
	rFFType := reflect.TypeOf(f).Elem()

	pluginVersions := make(map[string]string)
	for i := 0; i < rFFVal.NumField(); i++ {
		rFieldVal := rFFVal.Field(i)
		rFieldType := rFFType.Field(i)

		pluginId, hasPluginId := rFieldType.Tag.Lookup("plugin_id")
		if !hasPluginId {
			continue
		}

		pluginVersions[pluginId] = rFieldVal.String()
	}

	return pluginVersions
}

// ToMap returns the feature flags as a map[string]string
// Supports boolean and string feature flags.
func (f *FeatureFlags) ToMap() map[string]string {
	refStructVal := reflect.ValueOf(*f)
	refStructType := reflect.TypeOf(*f)
	ret := make(map[string]string)
	for i := 0; i < refStructVal.NumField(); i++ {
		refFieldVal := refStructVal.Field(i)
		if !refFieldVal.IsValid() {
			continue
		}
		refFieldType := refStructType.Field(i)
		switch refFieldType.Type.Kind() {
		case reflect.Bool:
			ret[refFieldType.Name] = strconv.FormatBool(refFieldVal.Bool())
		default:
			ret[refFieldType.Name] = refFieldVal.String()
		}
	}

	return ret
}
