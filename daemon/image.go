package daemon

import (
	"fmt"

	"github.com/docker/docker/builder"
	"github.com/docker/docker/image"
	"github.com/docker/docker/reference"
	"github.com/docker/docker/runconfig"
	containertypes "github.com/docker/engine-api/types/container"
	"github.com/Sirupsen/logrus"
	"time"
	"github.com/docker/docker/layer"
	"github.com/docker/distribution/digest"
	"github.com/docker/notary/tuf/data"
)

// ErrImageDoesNotExist is error returned when no image can be found for a reference.
type ErrImageDoesNotExist struct {
	RefOrID string
}

func (e ErrImageDoesNotExist) Error() string {
	return fmt.Sprintf("no such id: %s", e.RefOrID)
}

// GetImageID returns an image ID corresponding to the image referred to by
// refOrID.
func (daemon *Daemon) GetImageID(refOrID string) (image.ID, error) {
	id, ref, err := reference.ParseIDOrReference(refOrID)
	if err != nil {
		return "", err
	}
	if id != "" {
		if _, err := daemon.imageStore.Get(image.ID(id)); err != nil {
			return "", ErrImageDoesNotExist{refOrID}
		}
		return image.ID(id), nil
	}

	if id, err := daemon.referenceStore.Get(ref); err == nil {
		return id, nil
	}
	if tagged, ok := ref.(reference.NamedTagged); ok {
		if id, err := daemon.imageStore.Search(tagged.Tag()); err == nil {
			for _, namedRef := range daemon.referenceStore.References(id) {
				if namedRef.Name() == ref.Name() {
					return id, nil
				}
			}
		}
	}

	// Search based on ID
	if id, err := daemon.imageStore.Search(refOrID); err == nil {
		return id, nil
	}

	return "", ErrImageDoesNotExist{refOrID}
}

// GetImage returns an image corresponding to the image referred to by refOrID.
func (daemon *Daemon) GetImage(refOrID string) (*image.Image, error) {
	imgID, err := daemon.GetImageID(refOrID)
	if err != nil {
		return nil, err
	}
	return daemon.imageStore.Get(imgID)
}

// GetImageOnBuild looks up a Docker image referenced by `name`.
func (daemon *Daemon) GetImageOnBuild(name string) (builder.Image, error) {
	img, err := daemon.GetImage(name)
	if err != nil {
		return nil, err
	}
	return img, nil
}

// GetCachedImage returns the most recent created image that is a child
// of the image with imgID, that had the same config when it was
// created. nil is returned if a child cannot be found. An error is
// returned if the parent image cannot be found.
func (daemon *Daemon) GetCachedImage(imgID image.ID, config *containertypes.Config) (*image.Image, error) {
	// Loop on the children of the given image and check the config
	getMatch := func(siblings []image.ID) (*image.Image, error) {
		var match *image.Image
		for _, id := range siblings {
			img, err := daemon.imageStore.Get(id)
			if err != nil {
				return nil, fmt.Errorf("unable to find image %q", id)
			}

			if runconfig.Compare(&img.ContainerConfig, config) {
				// check for the most up to date match
				if match == nil || match.Created.Before(img.Created) {
					match = img
				}
			}
		}
		return match, nil
	}

	// In this case, this is `FROM scratch`, which isn't an actual image.
	if imgID == "" {
		images := daemon.imageStore.Map()
		var siblings []image.ID
		for id, img := range images {
			if img.Parent == imgID {
				siblings = append(siblings, id)
			}
		}
		return getMatch(siblings)
	}

	// find match from child images
	siblings := daemon.imageStore.Children(imgID)
	return getMatch(siblings)
}

type daemonImageCacheForBuild struct {
	// cacheFromImages here is a map of (provided) names to the actual images it represents
	cacheFromImages			map[string]*image.Image
	// cacheFromImageHistories also provides a map back to a historyWithSourceT struct
	cacheFromImageHistories	map[string]historyWithSourceT
	// daemon stores a reference to the daemon that backs this cache
	daemon 					*Daemon
}


func (daemon *Daemon) MakeImageCacheForBuild(cacheFrom []string) builder.ImageCacheForBuild {
	cache := &daemonImageCacheForBuild{
		daemon: 			daemon,
	}

	// for each cacheFrom image, set up the channels & coroutine for scrolling forward through
	// its history and comparing it to what's being built
	for _, cacheFromImageName := range cacheFrom {
		cacheFromImage, err := daemon.GetImage(cacheFromImageName)
		if err != nil {
			logrus.Warnf("Could not look up %s for cache resolution, skipping: %s", cacheFromImageName, err)
			continue
		}

		logrus.Infof("I found %s for %s", cacheFromImage.ID().String(), cacheFromImageName)
		cache.cacheFromImages[cacheFromImageName] = cacheFromImage
		cache.cacheFromImageHistories[cacheFromImageName] = makeHistoryWithSource(cacheFromImage)
	}

	return cache
}

// In the history array, we have pairs of (command, resultingLayerID). What we actually want to be able
// to compare is pairs of (sourceLayerID, command), and if we have a match, consult resultingLayerID.
// We also don't directly have source/resultingLayerID, but rather a boolean "did create new layer" flag.
// Define a struct to store this mapping for convenience.
type historyWithSourceT struct {
	// sourceLayerID is the layer on which the command was run. Empty digest if this is the first command or
	// if nothing has actually been added to the rootfs yet.
	sourceLayerID		layer.DiffID
	// cmd is the command which got run on sourceLayerID
	cmd 				string
	// resulingLayerID is the result of running cmd on sourceLayerID (might be the same as sourceLayerID)
	resultingLayerID	layer.DiffID
	// createdAt is the time the history entry was created
	createdAt			time.Time
}

func makeHistoryWithSource(image *image.Image) []historyWithSourceT {
	// Let's make those structs now
	historyWithSource := make([]historyWithSourceT, len(image.History))
	layerIndex := -1
	for i, h := range image.History {

		// previous is layerIndex from previous iteration
		if layerIndex == -1 {
			historyWithSource[i].sourceLayerID = digest.DigestSha256EmptyTar
		} else {
			historyWithSource[i].sourceLayerID = image.RootFS.DiffIDs[layerIndex]
		}

		// now increment, if needed, and look at the result layer ID
		if !h.EmptyLayer {
			layerIndex = layerIndex + 1
		}
		if layerIndex == -1 {
			historyWithSource[i].resultingLayerID = digest.DigestSha256EmptyTar
		} else {
			historyWithSource[i].resultingLayerID = image.RootFS.DiffIDs[layerIndex]
		}

		// Copy the other history entries over I'm interested in
		historyWithSource[i].cmd = h.CreatedBy
		historyWithSource[i].createdAt = h.Created
	}

	return historyWithSource
}

func cacheSearchCoroutine(data cacheCoroutineData)  {
	// Because a layer shasum does not include a hash of the parent in it, we need to compare
	// *all* of the previous layers we have iterated on with the layers in the image provided to
	// us in the request. Store a slice into data.cacheFromImage.RootFS.DiffIDs to represent this.
	var prevStepCachedLayers []layer.DiffID

	historyWithSource := makeHistoryWithSource(data.cacheFromImage)

	for _, h := range historyWithSource {
		// add prev to the list of all previous layers, if its not empty
		if h.sourceLayerID != digest.DigestSha256EmptyTar {
			prevStepCachedLayers = data.cacheFromImage.RootFS.DiffIDs[0:len(prevStepCachedLayers)]
		}

		req, ok := <-data.reqChan
		if !ok {
			// break will finish the goroutine
			break
		}


		// Compare with all previous layers using our set
		var matchesLayerIDs bool
		if len(prevStepCachedLayers) == len(req.prevLayerIDs) {
			matchesLayerIDs = true
			for i := 0; i <= len(prevStepCachedLayers); i++ {
				if prevStepCachedLayers[i] != req.prevLayerIDs[i] {
					matchesLayerIDs = false
					break
				}
			}
		} else {
			matchesLayerIDs = false
		}

		if req.nextCmd == h.cmd && matchesLayerIDs {
			data.resChan <- nextCachedLayerResponse{
				nextLayerID:	h.resultingLayerID,
				createdAt:		h.createdAt,
				cachedFrom:		data.cacheFromImageName,
			}
		} else {
			// Send a message telling our caller not to ask us again, then close
			// the channels
			data.resChan <- nextCachedLayerResponse{
				nextLayerID:	"",
			}
		}
	}
}

// GetCachedImageOnBuild returns a reference to a cached image whose parent equals `parent`
// and runconfig equals `cfg`. A cache miss is expected to return an empty ID and a nil error.
func (cache *daemonImageCacheForBuild) GetCachedImageOnBuild(imgID string, cfg *containertypes.Config) (string, error) {
	cachedImage, err := cache.daemon.GetCachedImage(image.ID(imgID), cfg)
	if err != nil {
		return "", err
	}
	if cachedImage != nil {
		// We found a cache hit using the old parent image method
		return cachedImage.ID().String(), nil
	}
	// We didn't find a cache hit using that method. Explore cacheFrom images for matching history
	parentImage, err := cache.daemon.GetImage(image.ID(imgID))
	if err != nil {
		return "", err
	}
	parentImageHistory := makeHistoryWithSource(parentImage)

	// For each thing we are caching from, see if it matches parentImageHistory
	type matchStruct struct {

	}
	matches := make([]matchStruct, 0, len(cache.cacheFromImages))
	for cacheFromName, cacheFromImage := range cache.cacheFromImages {
		if len(cache.cacheFromImageHistories[cacheFromName]) <= len(parentImageHistory) {
			// This won't really work - we have more steps than the cache from image has, so
			// there is no possibility of a match.
			continue
		}
	}
}

