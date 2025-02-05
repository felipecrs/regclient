package mod

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/regclient/regclient"
	"github.com/regclient/regclient/types/manifest"
	"github.com/regclient/regclient/types/ref"
)

// WithLayerReproducible modifies the layer with reproducible options.
// This currently configures users and groups with numeric ids.
func WithLayerReproducible() Opts {
	return func(dc *dagConfig, dm *dagManifest) error {
		dc.stepsLayerFile = append(dc.stepsLayerFile,
			func(c context.Context, rc *regclient.RegClient, rSrc, rTgt ref.Ref, dl *dagLayer, th *tar.Header, tr io.Reader) (*tar.Header, io.Reader, changes, error) {
				changed := false
				if th.Uname != "" {
					th.Uname = ""
					changed = true
				}
				if th.Gname != "" {
					th.Gname = ""
					changed = true
				}
				if changed {
					return th, tr, replaced, nil
				}
				return th, tr, unchanged, nil
			})
		return nil
	}
}

// WithLayerRmCreatedBy deletes a layer based on a regex of the created by field
// in the config history for that layer
func WithLayerRmCreatedBy(re regexp.Regexp) Opts {
	return func(dc *dagConfig, dm *dagManifest) error {
		dc.stepsManifest = append(dc.stepsManifest, func(c context.Context, rc *regclient.RegClient, rSrc, rTgt ref.Ref, dm *dagManifest) error {
			if dm.m.IsList() || dm.config.oc == nil {
				return nil
			}
			if dm.layers == nil || len(dm.layers) == 0 {
				return fmt.Errorf("no layers found")
			}
			delLayers := []int{}
			oc := dm.config.oc.GetConfig()
			i := 0
			for _, ch := range oc.History {
				if ch.EmptyLayer {
					continue
				}
				if re.Match([]byte(ch.CreatedBy)) {
					delLayers = append(delLayers, i)
				}
				i++
			}
			if len(delLayers) == 0 {
				return fmt.Errorf("no layers match expression: %s", re.String())
			}
			curLayer := 0
			curOrigLayer := 0
			for _, i := range delLayers {
				for {
					if len(dm.layers) <= curLayer {
						return fmt.Errorf("layers missing")
					}
					if dm.layers[curLayer].mod == added {
						curLayer++
						continue
					}
					if curOrigLayer == i {
						dm.layers[curLayer].mod = deleted
						break
					}
					curLayer++
					curOrigLayer++
				}
			}
			return nil
		})
		return nil
	}
}

// WithLayerRmIndex deletes a layer by index. The index starts at 0.
func WithLayerRmIndex(index int) Opts {
	return func(dc *dagConfig, dm *dagManifest) error {
		dc.stepsManifest = append(dc.stepsManifest, func(c context.Context, rc *regclient.RegClient, rSrc, rTgt ref.Ref, dm *dagManifest) error {
			if !dm.top || dm.m.IsList() || dm.config.oc == nil {
				return fmt.Errorf("remove layer by index requires v2 image manifest")
			}
			if dm.layers == nil || len(dm.layers) == 0 {
				return fmt.Errorf("no layers found")
			}
			curLayer := 0
			curOrigLayer := 0
			for {
				if len(dm.layers) <= curLayer {
					return fmt.Errorf("layer not found")
				}
				if dm.layers[curLayer].mod == added {
					curLayer++
					continue
				}
				if curOrigLayer == index {
					dm.layers[curLayer].mod = deleted
					break
				}
				curLayer++
				curOrigLayer++
			}
			return nil
		})
		return nil
	}
}

// WithLayerStripFile removes a file from within the layer tar
func WithLayerStripFile(file string) Opts {
	file = strings.Trim(file, "/")
	fileRE := regexp.MustCompile("^/?" + regexp.QuoteMeta(file) + "(/.*)?$")
	return func(dc *dagConfig, dm *dagManifest) error {
		dc.stepsLayerFile = append(dc.stepsLayerFile, func(c context.Context, rc *regclient.RegClient, rSrc, rTgt ref.Ref, dl *dagLayer, th *tar.Header, tr io.Reader) (*tar.Header, io.Reader, changes, error) {
			if fileRE.Match([]byte(th.Name)) {
				return th, tr, deleted, nil
			}
			return th, tr, unchanged, nil
		})
		return nil
	}
}

// WithLayerTimestamp sets the timestamp on files in the layers based on options
func WithLayerTimestamp(optTime OptTime) Opts {
	return func(dc *dagConfig, dm *dagManifest) error {
		if optTime.Set.IsZero() && optTime.FromLabel == "" {
			return fmt.Errorf("WithLayerTimestamp requires a time to set")
		}
		baseProcessed := false
		baseDigests := map[digest.Digest]bool{}
		// add base layers by count
		if optTime.BaseLayers > 0 {
			dl, err := layerGetBaseCount(optTime.BaseLayers, dm)
			if err != nil {
				return fmt.Errorf("failed to get base layers: %w", err)
			}
			for _, d := range dl {
				baseDigests[d] = true
			}
		}
		if optTime.FromLabel != "" {
			dc.stepsOCIConfig = append(dc.stepsOCIConfig, func(c context.Context, rc *regclient.RegClient, rSrc, rTgt ref.Ref, doc *dagOCIConfig) error {
				oc := doc.oc.GetConfig()
				tl, ok := oc.Config.Labels[optTime.FromLabel]
				if !ok {
					return fmt.Errorf("label not found: %s", optTime.FromLabel)
				}
				tNew, err := time.Parse(time.RFC3339, tl)
				if err != nil {
					// TODO: add fallbacks
					return fmt.Errorf("could not parse time %s from %s: %w", tl, optTime.FromLabel, err)
				}
				if !optTime.Set.IsZero() && !optTime.Set.Equal(tNew) {
					return fmt.Errorf("conflicting time labels found %s and %s", optTime.Set.String(), tNew.String())
				}
				optTime.Set = tNew
				return nil
			})
		}
		dc.stepsLayerFile = append(dc.stepsLayerFile,
			func(c context.Context, rc *regclient.RegClient, rSrc, rTgt ref.Ref, dl *dagLayer, th *tar.Header, tr io.Reader) (*tar.Header, io.Reader, changes, error) {
				if optTime.Set.IsZero() {
					return nil, nil, unchanged, fmt.Errorf("timestamp not available")
				}
				// for base ref, lookup all digests from base image to exclude
				if !baseProcessed {
					if !optTime.BaseRef.IsZero() {
						m, err := rc.ManifestGet(c, optTime.BaseRef)
						if err != nil {
							return nil, nil, unchanged, fmt.Errorf("failed to get base image: %w", err)
						}
						dl, err := layerGetBaseRef(c, rc, optTime.BaseRef, m)
						if err != nil {
							return nil, nil, unchanged, fmt.Errorf("failed to get base layers: %w", err)
						}
						for _, d := range dl {
							baseDigests[d] = true
						}
					}
					baseProcessed = true
				}
				// skip layers from base image
				if baseDigests[dl.desc.Digest] {
					return th, tr, unchanged, nil
				}
				if th == nil || tr == nil {
					return nil, nil, unchanged, fmt.Errorf("missing header or reader")
				}
				var ca, cc, cm bool
				// do not mod times that are currently zero, underlying tar format may not support changing
				if !th.AccessTime.IsZero() {
					th.AccessTime, ca = timeModOpt(th.AccessTime, optTime)
				}
				if !th.ChangeTime.IsZero() {
					th.ChangeTime, cc = timeModOpt(th.ChangeTime, optTime)
				}
				if !th.ModTime.IsZero() {
					th.ModTime, cm = timeModOpt(th.ModTime, optTime)
				}
				if ca || cc || cm {
					return th, tr, replaced, nil
				}
				return th, tr, unchanged, nil
			},
		)
		return nil
	}
}

func layerGetBaseCount(count int, dm *dagManifest) ([]digest.Digest, error) {
	dl := []digest.Digest{}
	for _, dmChild := range dm.manifests {
		dlChild, err := layerGetBaseCount(count, dmChild)
		if err != nil {
			return dl, err
		}
		dl = append(dl, dlChild...)
	}
	for i, layer := range dm.layers {
		if i < count {
			dl = append(dl, layer.desc.Digest)
		}
	}
	return dl, nil
}

func layerGetBaseRef(c context.Context, rc *regclient.RegClient, r ref.Ref, m manifest.Manifest) ([]digest.Digest, error) {
	dl := []digest.Digest{}
	if mi, ok := m.(manifest.Indexer); ok {
		ml, err := mi.GetManifestList()
		if err != nil {
			return dl, err
		}
		for _, d := range ml {
			mChild, err := rc.ManifestGet(c, r, regclient.WithManifestDesc(d))
			if err != nil {
				return dl, err
			}
			dlChild, err := layerGetBaseRef(c, rc, r, mChild)
			if err != nil {
				return dl, err
			}
			dl = append(dl, dlChild...)
		}
	}
	if mi, ok := m.(manifest.Imager); ok {
		layers, err := mi.GetLayers()
		if err != nil {
			return dl, err
		}
		for _, l := range layers {
			dl = append(dl, l.Digest)
		}
	}
	return dl, nil
}

// WithLayerTimestampFromLabel sets the max layer timestamp based on a label in the image
//
// Deprecated: replace with WithLayerTimestamp
func WithLayerTimestampFromLabel(label string) Opts {
	t := time.Time{}
	return func(dc *dagConfig, dm *dagManifest) error {
		dc.stepsOCIConfig = append(dc.stepsOCIConfig, func(c context.Context, rc *regclient.RegClient, rSrc, rTgt ref.Ref, doc *dagOCIConfig) error {
			oc := doc.oc.GetConfig()
			tl, ok := oc.Config.Labels[label]
			if !ok {
				return fmt.Errorf("label not found: %s", label)
			}
			tNew, err := time.Parse(time.RFC3339, tl)
			if err != nil {
				// TODO: add fallbacks
				return fmt.Errorf("could not parse time %s from %s: %w", tl, label, err)
			}
			if !t.IsZero() && !t.Equal(tNew) {
				return fmt.Errorf("conflicting time labels found %s and %s", t.String(), tNew.String())
			}
			t = tNew
			return nil
		})
		dc.stepsLayerFile = append(dc.stepsLayerFile,
			func(c context.Context, rc *regclient.RegClient, rSrc, rTgt ref.Ref, dl *dagLayer, th *tar.Header, tr io.Reader) (*tar.Header, io.Reader, changes, error) {
				if t.IsZero() {
					return nil, nil, unchanged, fmt.Errorf("timestamp not available")
				}
				changed := false
				if th == nil || tr == nil {
					return nil, nil, unchanged, fmt.Errorf("missing header or reader")
				}
				if t.Before(th.AccessTime) {
					th.AccessTime = t
					changed = true
				}
				if t.Before(th.ChangeTime) {
					th.ChangeTime = t
					changed = true
				}
				if t.Before(th.ModTime) {
					th.ModTime = t
					changed = true
				}
				if changed {
					return th, tr, replaced, nil
				}
				return th, tr, unchanged, nil
			},
		)
		return nil
	}
}

// WithLayerTimestampMax ensures no file timestamps are after specified time
//
// Deprecated: replace with WithLayerTimestamp
func WithLayerTimestampMax(t time.Time) Opts {
	return WithLayerTimestamp(OptTime{
		Set:   t,
		After: t,
	})
}

// WithFileTarTime processes a tar file within a layer and adjusts the timestamps according to optTime
func WithFileTarTime(name string, optTime OptTime) Opts {
	name = strings.TrimPrefix(name, "/")
	return func(dc *dagConfig, dm *dagManifest) error {
		if optTime.Set.IsZero() && optTime.FromLabel == "" {
			return fmt.Errorf("WithFileTarTime requires a time to set")
		}
		baseProcessed := false
		baseDigests := map[digest.Digest]bool{}
		// add base layers by count
		if optTime.BaseLayers > 0 {
			dl, err := layerGetBaseCount(optTime.BaseLayers, dm)
			if err != nil {
				return fmt.Errorf("failed to get base layers: %w", err)
			}
			for _, d := range dl {
				baseDigests[d] = true
			}
		}
		if optTime.FromLabel != "" {
			dc.stepsOCIConfig = append(dc.stepsOCIConfig, func(c context.Context, rc *regclient.RegClient, rSrc, rTgt ref.Ref, doc *dagOCIConfig) error {
				oc := doc.oc.GetConfig()
				tl, ok := oc.Config.Labels[optTime.FromLabel]
				if !ok {
					return fmt.Errorf("label not found: %s", optTime.FromLabel)
				}
				tNew, err := time.Parse(time.RFC3339, tl)
				if err != nil {
					// TODO: add fallbacks
					return fmt.Errorf("could not parse time %s from %s: %w", tl, optTime.FromLabel, err)
				}
				if !optTime.Set.IsZero() && !optTime.Set.Equal(tNew) {
					return fmt.Errorf("conflicting time labels found %s and %s", optTime.Set.String(), tNew.String())
				}
				optTime.Set = tNew
				return nil
			})
		}
		dc.stepsLayerFile = append(dc.stepsLayerFile, func(ctx context.Context, rc *regclient.RegClient, rSrc, rTgt ref.Ref, dl *dagLayer, th *tar.Header, tr io.Reader) (*tar.Header, io.Reader, changes, error) {
			if optTime.Set.IsZero() {
				return nil, nil, unchanged, fmt.Errorf("timestamp not available")
			}
			// for base ref, lookup all digests from base image to exclude
			if !baseProcessed {
				if !optTime.BaseRef.IsZero() {
					m, err := rc.ManifestGet(ctx, optTime.BaseRef)
					if err != nil {
						return nil, nil, unchanged, fmt.Errorf("failed to get base image: %w", err)
					}
					dl, err := layerGetBaseRef(ctx, rc, optTime.BaseRef, m)
					if err != nil {
						return nil, nil, unchanged, fmt.Errorf("failed to get base layers: %w", err)
					}
					for _, d := range dl {
						baseDigests[d] = true
					}
				}
				baseProcessed = true
			}
			// skip layers from base image
			if baseDigests[dl.desc.Digest] {
				return th, tr, unchanged, nil
			}
			if th == nil || tr == nil {
				return nil, nil, unchanged, fmt.Errorf("missing header or reader")
			}
			// check the header for a matching filename
			if th.Name != name {
				return th, tr, unchanged, nil
			}
			// read contents into a temporary file, adjusting included timestamps, track if any timestamps are changed
			tmpFile, err := os.CreateTemp("", "regclient.*")
			if err != nil {
				return th, tr, unchanged, err
			}
			// TODO: detect and handle compression
			changed := false
			tmpName := tmpFile.Name()
			fsTR := tar.NewReader(tr)
			fsTW := tar.NewWriter(tmpFile)
			defer fsTW.Close()
			for {
				fsTH, err := fsTR.Next()
				if err != nil {
					if errors.Is(err, io.EOF) {
						break
					}
					return th, tr, unchanged, err
				}
				var ca, cc, cm bool
				// do not mod times that are currently zero, underlying tar format may not support changing
				if !fsTH.AccessTime.IsZero() {
					fsTH.AccessTime, ca = timeModOpt(fsTH.AccessTime, optTime)
				}
				if !fsTH.ChangeTime.IsZero() {
					fsTH.ChangeTime, cc = timeModOpt(fsTH.ChangeTime, optTime)
				}
				if !fsTH.ModTime.IsZero() {
					fsTH.ModTime, cm = timeModOpt(fsTH.ModTime, optTime)
				}
				changed = changed || ca || cc || cm
				err = fsTW.WriteHeader(fsTH)
				if err != nil {
					return th, tr, unchanged, err
				}
				if fsTH.Size > 0 {
					_, err = io.CopyN(fsTW, fsTR, fsTH.Size)
					if err != nil {
						return th, tr, unchanged, err
					}
				}
			}
			err = fsTW.Close()
			if err != nil {
				return th, tr, unchanged, err
			}
			// return a reader that reads from the temporary file and deletes it when finished
			//#nosec G304 filename is from previous CreateTemp
			tmpFH, err := os.Open(tmpName)
			if err != nil {
				return th, tr, unchanged, err
			}
			fi, err := tmpFH.Stat()
			if err != nil {
				return th, tr, unchanged, err
			}
			th.Size = fi.Size()
			tmpR := tmpReader{
				file:     tmpFH,
				remain:   th.Size,
				filename: tmpName,
			}
			if changed {
				return th, &tmpR, replaced, nil
			}
			return th, &tmpR, unchanged, nil
		})
		return nil
	}
}

// WithFileTarTimeMax processes a tar file within a layer and rewrites the contents with a max timestamp
//
// Deprecated: replace with WithFileTarTime
func WithFileTarTimeMax(name string, t time.Time) Opts {
	return WithFileTarTime(name, OptTime{
		Set:   t,
		After: t,
	})
}

type tmpReader struct {
	file     *os.File
	remain   int64
	filename string
}

// Read for tmpReader passes through the read and deletes the tmp file when the read completes
func (t *tmpReader) Read(p []byte) (int, error) {
	if t.file == nil {
		return 0, io.EOF
	}
	size, err := t.file.Read(p)
	t.remain -= int64(size)
	if err != nil || t.remain <= 0 {
		// cleanup on last read or any errors, intentionally ignoring any other errors
		_ = t.file.Close()
		_ = os.Remove(t.filename)
		t.file = nil
	}
	return size, err
}
