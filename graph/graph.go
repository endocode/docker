package graph

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/appc/spec/schema"
	"github.com/docker/docker/daemon/graphdriver"
	"github.com/docker/docker/dockerversion"
	"github.com/docker/docker/image"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/truncindex"
	"github.com/docker/docker/runconfig"
	"github.com/docker/docker/utils"
	"github.com/docker/docker/vendor/src/code.google.com/p/go/src/pkg/archive/tar"
)

// A Graph is a store for versioned filesystem images and the relationship between them.
type Graph struct {
	Root    string
	idIndex *truncindex.TruncIndex
	driver  graphdriver.Driver
}

// NewGraph instantiates a new graph at the given root path in the filesystem.
// `root` will be created if it doesn't exist.
func NewGraph(root string, driver graphdriver.Driver) (*Graph, error) {
	abspath, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	// Create the root directory if it doesn't exists
	if err := os.MkdirAll(root, 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}

	graph := &Graph{
		Root:    abspath,
		idIndex: truncindex.NewTruncIndex([]string{}),
		driver:  driver,
	}
	if err := graph.restore(); err != nil {
		return nil, err
	}
	return graph, nil
}

func (graph *Graph) restore() error {
	dir, err := ioutil.ReadDir(graph.Root)
	if err != nil {
		return err
	}
	var ids = []string{}
	for _, v := range dir {
		id := v.Name()
		if graph.driver.Exists(id) {
			ids = append(ids, id)
		}
	}
	graph.idIndex = truncindex.NewTruncIndex(ids)
	log.Debugf("Restored %d elements", len(dir))
	return nil
}

// FIXME: Implement error subclass instead of looking at the error text
// Note: This is the way golang implements os.IsNotExists on Plan9
func (graph *Graph) IsNotExist(err error) bool {
	return err != nil && (strings.Contains(strings.ToLower(err.Error()), "does not exist") || strings.Contains(strings.ToLower(err.Error()), "no such"))
}

// Exists returns true if an image is registered at the given id.
// If the image doesn't exist or if an error is encountered, false is returned.
func (graph *Graph) Exists(id string) bool {
	if _, err := graph.Get(id); err != nil {
		return false
	}
	return true
}

func (graph *Graph) LoadACIManifest(root string) (*schema.ImageManifest, error) {
	return getManifestFromDir(root)
}

func (graph *Graph) GetACI(name string) (string, *schema.ImageManifest, error) {
	var manifest *schema.ImageManifest
	id, err := graph.idIndex.Get(name)
	if err != nil {
		return "", nil, fmt.Errorf("could not find image: %v", err)
	}
	manifest, err = graph.LoadACIManifest(graph.ImageRoot(id))
	return id, manifest, err
}

// Get returns the image with the given id, or an error if the image doesn't exist.
func (graph *Graph) Get(name string) (*image.Image, error) {
	id, err := graph.idIndex.Get(name)
	if err != nil {
		return nil, fmt.Errorf("could not find image: %v", err)
	}
	img, err := image.LoadImage(graph.ImageRoot(id))
	if err != nil {
		return nil, err
	}
	if img.ID != id {
		return nil, fmt.Errorf("Image stored at '%s' has wrong id '%s'", id, img.ID)
	}
	img.SetGraph(graph)

	if img.Size < 0 {
		size, err := graph.driver.DiffSize(img.ID, img.Parent)
		if err != nil {
			return nil, fmt.Errorf("unable to calculate size of image id %q: %s", img.ID, err)
		}

		img.Size = size
		if err := img.SaveSize(graph.ImageRoot(id)); err != nil {
			return nil, err
		}
	}
	return img, nil
}

// Create creates a new image and registers it in the graph.
func (graph *Graph) Create(layerData archive.ArchiveReader, containerID, containerImage, comment, author string, containerConfig, config *runconfig.Config) (*image.Image, error) {
	img := &image.Image{
		ID:            utils.GenerateRandomID(),
		Comment:       comment,
		Created:       time.Now().UTC(),
		DockerVersion: dockerversion.VERSION,
		Author:        author,
		Config:        config,
		Architecture:  runtime.GOARCH,
		OS:            runtime.GOOS,
	}

	if containerID != "" {
		img.Parent = containerImage
		img.Container = containerID
		img.ContainerConfig = *containerConfig
	}

	if err := graph.Register(img, layerData); err != nil {
		return nil, err
	}
	return img, nil
}

func (graph *Graph) RegisterACI(aci io.Reader) (*schema.ImageManifest, string, error) {
	tmp, err := graph.Mktemp("")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(tmp)

	manifest, id, err := untarACI(tmp, aci)
	if err != nil {
		return nil, "", err
	}

	// check if the layer already exists
	_, err = os.Stat(graph.ImageRoot(id))
	if !os.IsNotExist(err) {
		return manifest, id, nil
	}

	layerFile, err := createLayerTar(tmp)
	if err != nil {
		return nil, "", err
	}
	defer layerFile.Close()
	if err := os.RemoveAll(path.Join(tmp, "rootfs")); err != nil {
		return nil, "", err
	}

	// FIXME: ACI can have dependencies. They are not supported yet.
	// At the moment, the parent is not specified (empty string)
	if err := graph.driver.Create(id, ""); err != nil {
		return nil, "", err
	}
	if _, err := graph.driver.ApplyDiff(id, "", archive.ArchiveReader(layerFile)); err != nil {
		return nil, "", err
	}
	if err := os.Rename(tmp, graph.ImageRoot(id)); err != nil {
		return nil, "", err
	}
	graph.idIndex.Add(id)

	return manifest, id, nil
}

type DeleteOnClose struct {
	file     *os.File
	filename string
}

func (doc *DeleteOnClose) Read(p []byte) (int, error) {
	return doc.file.Read(p)
}

func (doc *DeleteOnClose) Close() error {
	if err := doc.file.Close(); err != nil {
		return err
	}
	if err := os.Remove(doc.filename); err != nil {
		return err
	}
	return nil
}

// storeDecompressed stores the aci on disk as uncompressed tar,
// returns a reader to it and its sha256sum.
func storeDecompressed(target string, aci io.Reader) (io.ReadCloser, string, error) {
	decompressed, err := archive.DecompressStream(aci)
	if err != nil {
		return nil, "", err
	}
	defer decompressed.Close()

	hasher := sha256.New()
	teeReader := io.TeeReader(decompressed, hasher)
	aciFilename := path.Join(target, "aci.tar")
	tarFile, err := os.Create(aciFilename)

	if err != nil {
		return nil, "", err
	}
	_, err = io.Copy(tarFile, teeReader)
	if err != nil {
		return nil, "", err
	}
	if _, err := tarFile.Seek(0, 0); err != nil {
		return nil, "", err
	}

	doc := &DeleteOnClose{file: tarFile, filename: aciFilename}
	return doc, fmt.Sprintf("%x", hasher.Sum(nil)), nil
}

func getManifestFromDir(root string) (*schema.ImageManifest, error) {
	manifest := &schema.ImageManifest{}
	jsonBytes, err := ioutil.ReadFile(path.Join(root, "manifest"))
	if err != nil {
		return nil, err
	}
	if err := manifest.UnmarshalJSON(jsonBytes); err != nil {
		return nil, err
	}
	return manifest, nil
}

// validateUntarredACI checks if manifest is a file and a proper ACI
// manifest and tests if rootfs directory exists.
func validateUntarredACI(target string) (*schema.ImageManifest, error) {
	manifest, err := getManifestFromDir(target)
	if err != nil {
		return nil, err
	}

	rootfsPath := path.Join(target, "rootfs")
	fi, err := os.Lstat(rootfsPath)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsDir() {
		return nil, errors.New("invalid ACI - rootfs should be a directory")
	}

	return manifest, nil
}

func untarACI(target string, aci io.Reader) (*schema.ImageManifest, string, error) {
	tarFile, hash, err := storeDecompressed(target, aci)
	if err != nil {
		return nil, "", err
	}
	defer tarFile.Close()

	tarReader := tar.NewReader(tarFile)
	for {
		header, err := tarReader.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, "", err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := writeADir(target, header); err != nil {
				return nil, "", err
			}
		case tar.TypeReg:
			if err := writeAFile(target, header, tarReader); err != nil {
				return nil, "", err
			}
		default:
			//TODO: Handle symlinks. Maybe all types?
		}
	}

	if manifest, err := validateUntarredACI(target); err != nil {
		return nil, "", err
	} else {
		return manifest, hash, nil
	}
}

func writeADir(target string, header *tar.Header) error {
	dir := path.Join(target, header.Name)
	return os.MkdirAll(dir, os.FileMode(header.Mode))
}

func writeAFile(target string, header *tar.Header, reader io.Reader) error {
	filename := path.Join(target, header.Name)
	writer, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer writer.Close()
	_, err = io.Copy(writer, reader)
	if err != nil {
		return err
	}
	return os.Chmod(filename, os.FileMode(header.Mode))
}

type tarPacker struct {
	writer *tar.Writer
	root   string
}

func (packer *tarPacker) Pack() error {
	return filepath.Walk(packer.root, packer.walkAndPack)
}

func (packer *tarPacker) walkAndPack(path string, info os.FileInfo, err error) error {
	if err != nil {
		return err
	}
	if info.Mode().IsDir() {
		return nil
	}
	newPath := path[len(packer.root):]
	// TODO: handle symlinks
	header, ferr := tar.FileInfoHeader(info, newPath)
	if ferr != nil {
		return ferr
	}
	header.Name = newPath
	if ferr := packer.writer.WriteHeader(header); ferr != nil {
		return ferr
	}
	f, ferr := os.Open(path)
	if ferr != nil {
		return ferr
	}
	if _, ferr := io.Copy(packer.writer, f); ferr != nil {
		return ferr
	}
	return nil
}

func createLayerTar(target string) (archive.Archive, error) {
	layerFile, err := os.Create(path.Join(target, "layer.tar"))
	if err != nil {
		return nil, err
	}
	tarWriter := tar.NewWriter(layerFile)
	rootfsPath := path.Join(target, "rootfs")
	packer := &tarPacker{tarWriter, rootfsPath}
	if err := packer.Pack(); err != nil {
		return nil, err
	}
	layerFile.Seek(0, 0)
	return archive.Archive(layerFile), nil
}

// Register imports a pre-existing image into the graph.
func (graph *Graph) Register(img *image.Image, layerData archive.ArchiveReader) (err error) {
	defer func() {
		// If any error occurs, remove the new dir from the driver.
		// Don't check for errors since the dir might not have been created.
		// FIXME: this leaves a possible race condition.
		if err != nil {
			graph.driver.Remove(img.ID)
		}
	}()
	if err := utils.ValidateID(img.ID); err != nil {
		return err
	}
	// (This is a convenience to save time. Race conditions are taken care of by os.Rename)
	if graph.Exists(img.ID) {
		return fmt.Errorf("Image %s already exists", img.ID)
	}

	// Ensure that the image root does not exist on the filesystem
	// when it is not registered in the graph.
	// This is common when you switch from one graph driver to another
	if err := os.RemoveAll(graph.ImageRoot(img.ID)); err != nil && !os.IsNotExist(err) {
		return err
	}

	// If the driver has this ID but the graph doesn't, remove it from the driver to start fresh.
	// (the graph is the source of truth).
	// Ignore errors, since we don't know if the driver correctly returns ErrNotExist.
	// (FIXME: make that mandatory for drivers).
	graph.driver.Remove(img.ID)

	tmp, err := graph.Mktemp("")
	defer os.RemoveAll(tmp)
	if err != nil {
		return fmt.Errorf("Mktemp failed: %s", err)
	}

	// Create root filesystem in the driver
	if err := graph.driver.Create(img.ID, img.Parent); err != nil {
		return fmt.Errorf("Driver %s failed to create image rootfs %s: %s", graph.driver, img.ID, err)
	}
	// Apply the diff/layer
	img.SetGraph(graph)
	if err := image.StoreImage(img, layerData, tmp); err != nil {
		return err
	}
	// Commit
	if err := os.Rename(tmp, graph.ImageRoot(img.ID)); err != nil {
		return err
	}
	graph.idIndex.Add(img.ID)
	return nil
}

// TempLayerArchive creates a temporary archive of the given image's filesystem layer.
//   The archive is stored on disk and will be automatically deleted as soon as has been read.
//   If output is not nil, a human-readable progress bar will be written to it.
//   FIXME: does this belong in Graph? How about MktempFile, let the caller use it for archives?
func (graph *Graph) TempLayerArchive(id string, sf *utils.StreamFormatter, output io.Writer) (*archive.TempArchive, error) {
	image, err := graph.Get(id)
	if err != nil {
		return nil, err
	}
	tmp, err := graph.Mktemp("")
	if err != nil {
		return nil, err
	}
	a, err := image.TarLayer()
	if err != nil {
		return nil, err
	}
	progress := utils.ProgressReader(a, 0, output, sf, false, utils.TruncateID(id), "Buffering to disk")
	defer progress.Close()
	return archive.NewTempArchive(progress, tmp)
}

// Mktemp creates a temporary sub-directory inside the graph's filesystem.
func (graph *Graph) Mktemp(id string) (string, error) {
	dir := path.Join(graph.Root, "_tmp", utils.GenerateRandomID())
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return dir, nil
}

func (graph *Graph) newTempFile() (*os.File, error) {
	tmp, err := graph.Mktemp("")
	if err != nil {
		return nil, err
	}
	return ioutil.TempFile(tmp, "")
}

func bufferToFile(f *os.File, src io.Reader) (int64, error) {
	n, err := io.Copy(f, src)
	if err != nil {
		return n, err
	}
	if err = f.Sync(); err != nil {
		return n, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return n, err
	}
	return n, nil
}

// setupInitLayer populates a directory with mountpoints suitable
// for bind-mounting dockerinit into the container. The mountpoint is simply an
// empty file at /.dockerinit
//
// This extra layer is used by all containers as the top-most ro layer. It protects
// the container from unwanted side-effects on the rw layer.
func SetupInitLayer(initLayer string) error {
	for pth, typ := range map[string]string{
		"/dev/pts":         "dir",
		"/dev/shm":         "dir",
		"/proc":            "dir",
		"/sys":             "dir",
		"/.dockerinit":     "file",
		"/.dockerenv":      "file",
		"/etc/resolv.conf": "file",
		"/etc/hosts":       "file",
		"/etc/hostname":    "file",
		"/dev/console":     "file",
		"/etc/mtab":        "/proc/mounts",
	} {
		parts := strings.Split(pth, "/")
		prev := "/"
		for _, p := range parts[1:] {
			prev = path.Join(prev, p)
			syscall.Unlink(path.Join(initLayer, prev))
		}

		if _, err := os.Stat(path.Join(initLayer, pth)); err != nil {
			if os.IsNotExist(err) {
				if err := os.MkdirAll(path.Join(initLayer, path.Dir(pth)), 0755); err != nil {
					return err
				}
				switch typ {
				case "dir":
					if err := os.MkdirAll(path.Join(initLayer, pth), 0755); err != nil {
						return err
					}
				case "file":
					f, err := os.OpenFile(path.Join(initLayer, pth), os.O_CREATE, 0755)
					if err != nil {
						return err
					}
					f.Close()
				default:
					if err := os.Symlink(typ, path.Join(initLayer, pth)); err != nil {
						return err
					}
				}
			} else {
				return err
			}
		}
	}

	// Layer is ready to use, if it wasn't before.
	return nil
}

// Check if given error is "not empty".
// Note: this is the way golang does it internally with os.IsNotExists.
func isNotEmpty(err error) bool {
	switch pe := err.(type) {
	case nil:
		return false
	case *os.PathError:
		err = pe.Err
	case *os.LinkError:
		err = pe.Err
	}
	return strings.Contains(err.Error(), " not empty")
}

// Delete atomically removes an image from the graph.
func (graph *Graph) Delete(name string) error {
	id, err := graph.idIndex.Get(name)
	if err != nil {
		return err
	}
	tmp, err := graph.Mktemp("")
	graph.idIndex.Delete(id)
	if err == nil {
		err = os.Rename(graph.ImageRoot(id), tmp)
		// On err make tmp point to old dir and cleanup unused tmp dir
		if err != nil {
			os.RemoveAll(tmp)
			tmp = graph.ImageRoot(id)
		}
	} else {
		// On err make tmp point to old dir for cleanup
		tmp = graph.ImageRoot(id)
	}
	// Remove rootfs data from the driver
	graph.driver.Remove(id)
	// Remove the trashed image directory
	return os.RemoveAll(tmp)
}

// MapACI returns a list of all ACI images in the graph, addressable by ID.
func (graph *Graph) MapACI(repo map[string]string) (map[string]*schema.ImageManifest, error) {
	images := make(map[string]*schema.ImageManifest)
	err := graph.walkAllACI(func(image *schema.ImageManifest) {
		id, ok := repo[string(image.Name)]
		if ok {
			images[id] = image
		}
	})
	if err != nil {
		return nil, err
	}
	return images, nil
}

// Map returns a list of all docker images in the graph, addressable by ID.
func (graph *Graph) Map() (map[string]*image.Image, error) {
	images := make(map[string]*image.Image)
	err := graph.walkAll(func(image *image.Image) {
		images[image.ID] = image
	})
	if err != nil {
		return nil, err
	}
	return images, nil
}

// walkAllACI iterates over each ACI image in the graph, and passes it to a handler.
// The walking order is undetermined.
func (graph *Graph) walkAllACI(handler func(*schema.ImageManifest)) error {
	files, err := ioutil.ReadDir(graph.Root)
	if err != nil {
		return err
	}
	for _, st := range files {
		if _, img, err := graph.GetACI(st.Name()); err != nil {
			// Skip image
			continue
		} else if handler != nil {
			handler(img)
		}
	}
	return nil
}

// walkAll iterates over each image in the graph, and passes it to a handler.
// The walking order is undetermined.
func (graph *Graph) walkAll(handler func(*image.Image)) error {
	files, err := ioutil.ReadDir(graph.Root)
	if err != nil {
		return err
	}
	for _, st := range files {
		if img, err := graph.Get(st.Name()); err != nil {
			// Skip image
			continue
		} else if handler != nil {
			handler(img)
		}
	}
	return nil
}

// ByParentACI returns a lookup table of images by their parent.
// If an image of id ID has 3 children images, then the value for key ID
// will be a list of 3 images.
// If an image has no children, it will not have an entry in the table.
//
// FIXME(ACI):
// It is rather broken, because we retrieve parents based on names
// instead of ids. Getting a parent by name might return different
// image when it was actually created.
//
// We need to store parent ids in image manifest along with image id.
func (graph *Graph) ByParentACI(repo map[string]string) (map[string][]*schema.ImageManifest, error) {
	byParent := make(map[string][]*schema.ImageManifest)
	err := graph.walkAllACI(func(img *schema.ImageManifest) {
		for _, dep := range img.Dependencies {
			_, parent, err := graph.GetACI(string(dep.App))
			if err != nil {
				continue
			}
			if id, ok := repo[string(parent.Name)]; ok {
				if children, exists := byParent[id]; exists {
					byParent[id] = append(children, img)
				} else {
					byParent[id] = []*schema.ImageManifest{img}
				}
			}
		}
	})
	return byParent, err
}

// ByParent returns a lookup table of images by their parent.
// If an image of id ID has 3 children images, then the value for key ID
// will be a list of 3 images.
// If an image has no children, it will not have an entry in the table.
func (graph *Graph) ByParent() (map[string][]*image.Image, error) {
	byParent := make(map[string][]*image.Image)
	err := graph.walkAll(func(img *image.Image) {
		parent, err := graph.Get(img.Parent)
		if err != nil {
			return
		}
		if children, exists := byParent[parent.ID]; exists {
			byParent[parent.ID] = append(children, img)
		} else {
			byParent[parent.ID] = []*image.Image{img}
		}
	})
	return byParent, err
}

// HeadsACI returns all ACI heads in the graph, keyed by id.
// A head is an image which is not the parent of another image in the graph.
func (graph *Graph) HeadsACI(repo map[string]string) (map[string]*schema.ImageManifest, error) {
	heads := make(map[string]*schema.ImageManifest)
	byParent, err := graph.ByParentACI(repo)
	if err != nil {
		return nil, err
	}
	err = graph.walkAllACI(func(image *schema.ImageManifest) {
		// If it's not in the byParent lookup table, then
		// it's not a parent -> so it's a head!
		if id, ok := repo[string(image.Name)]; ok {
			if _, exists := byParent[id]; !exists {
				heads[id] = image
			}
		}
	})
	return heads, err
}

// Heads returns all heads in the graph, keyed by id.
// A head is an image which is not the parent of another image in the graph.
func (graph *Graph) Heads() (map[string]*image.Image, error) {
	heads := make(map[string]*image.Image)
	byParent, err := graph.ByParent()
	if err != nil {
		return nil, err
	}
	err = graph.walkAll(func(image *image.Image) {
		// If it's not in the byParent lookup table, then
		// it's not a parent -> so it's a head!
		if _, exists := byParent[image.ID]; !exists {
			heads[image.ID] = image
		}
	})
	return heads, err
}

func (graph *Graph) ImageRoot(id string) string {
	return path.Join(graph.Root, id)
}

func (graph *Graph) Driver() graphdriver.Driver {
	return graph.driver
}
