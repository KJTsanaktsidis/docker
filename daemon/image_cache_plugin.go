package daemon

import (
    "github.com/docker/docker/pkg/plugins"
    containertypes "github.com/docker/engine-api/types/container"
)

const (
    WantsCachedImage = "ImageCachePlugin.WantsCachedImage"
)

type WantsCachedImageRequest struct (
    // The parent image we are looking for a child of...
    ParentImageId string `json:"ParentImageId,omitempty"`

    // ...which matches this config
    ContainerConfig containertypes.Config `json:"ContianerConfig,omitempty"`
)

type WantsCachedImageResponse struct (
    // The image id that was found, or nil if it didn't find one
    ImageId string `json:"ImageId,omitempty"`

    // Err stores a message in case there's an error
	Err string `json:"Err,omitempty"`
)

type ImageCachePlugin interface {
    // Name returns the registered plugin name
	Name() string

    // WantsCachedImage tells the plugin that we want an image specified by our request.
    // The plugin is responsible for using the docker API to put this image in the local image store.
    // It then tells us whether or not it found such a thing, and what the ID is if it did.
    WantsCachedImage(*WantsCachedImageRequest) (*WantsCachedImageResponse, error)
}