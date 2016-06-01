package svfs

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/xlucas/swift"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"golang.org/x/net/context"
)

const (
	DirContentType  = "application/directory"
	ObjContentType  = "application/octet-stream"
	LinkContentType = "application/link"
	AutoContent     = "X-Detect-Content-Type"
)

var (
	FolderRegex      = regexp.MustCompile("^.+/$")
	SubdirRegex      = regexp.MustCompile(".*/.*$")
	SegmentPathRegex = regexp.MustCompile("^([^/]+)/(.*)$")
	DirectoryCache   = NewCache()
	ChangeCache      = NewSimpleCache()
	DirectoryLister  = new(Lister)
)

// Directory represents a standard directory entry.
type Directory struct {
	apex bool
	name string
	path string
	so   *swift.Object
	sh   swift.Headers
	c    *swift.Container
	cs   *swift.Container
}

// Attr fills file attributes of a directory within the current context.
func (d *Directory) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | os.FileMode(DefaultMode)
	a.Gid = uint32(DefaultGID)
	a.Uid = uint32(DefaultUID)
	a.Size = uint64(BlockSize)

	if d.so != nil {
		a.Mtime = getMtime(d.so, d.sh)
		a.Ctime = a.Mtime
		a.Crtime = a.Mtime
	}

	return nil
}

// Create makes a new object node represented by a file. It returns
// an object node and an opened file handle.
func (d *Directory) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (fs.Node, fs.Handle, error) {
	// Create an empty object in swift
	path := d.path + req.Name

	// New node
	node := &Object{name: req.Name, path: path, c: d.c, cs: d.cs}

	err := SwiftConnection.ObjectPutBytes(node.c.Name, node.path, nil, "")
	if err != nil {
		return nil, nil, err
	}

	// Get object handler
	fh, err := node.open(req.Flags, &resp.Flags)
	if err != nil {
		return nil, nil, err
	}

	// Get object info
	obj := &swift.Object{
		Name:         path,
		Bytes:        0,
		LastModified: time.Now(),
	}

	node.so = obj
	node.sh = map[string]string{}

	// Cache it
	DirectoryCache.Set(d.c.Name, d.path, req.Name, node)

	return node, fh, nil
}

// Export gives a direntry for the current directory node.
func (d *Directory) Export() fuse.Dirent {
	return fuse.Dirent{
		Name: d.name,
		Type: fuse.DT_Dir,
	}
}

// ReadDirAll reads the content of a directory and returns a
// list of children nodes as direntries, using/filling the
// cache of nodes.
func (d *Directory) ReadDirAll(ctx context.Context) (direntries []fuse.Dirent, err error) {
	var (
		dirs  = make(map[string]bool)
		tasks = make(chan Node, ListerConcurrency)
		count = 0
	)

	defer close(tasks)

	// Cache check
	if _, nodes := DirectoryCache.GetAll(d.c.Name, d.path); nodes != nil {
		for _, node := range nodes {
			direntries = append(direntries, node.Export())
		}
		return direntries, nil
	}

	// Fetch objects
	objects, err := SwiftConnection.ObjectsAll(d.c.Name, &swift.ObjectsOpts{
		Delimiter: '/',
		Prefix:    d.path,
	})
	if err != nil {
		return nil, err
	}

	var children = make(map[string]Node)

	// Fill cache
	for _, object := range objects {
		var (
			child    Node
			o        = object
			path     = object.Name
			fileName = strings.TrimSuffix(strings.TrimPrefix(o.Name, d.path), "/")
		)

		// This is a symlink
		if isSymlink(o, d.path) {
			child = &Symlink{path: path, name: fileName, c: d.c, so: &o, sh: swift.Headers{}, p: d}
			DirectoryLister.AddTask(child, tasks)
			child = nil
			count++
			goto finish
		}

		// This is a standard directory
		if isDirectory(o, d.path) {
			if !strings.HasSuffix(o.Name, "/") {
				path += "/"
			}
			child = &Directory{c: d.c, cs: d.cs, so: &o, sh: swift.Headers{}, path: path, name: fileName}
			dirs[fileName] = true
			goto finish
		}

		// This is a pseudo directory. Add it only if the real directory is missing
		if isPseudoDirectory(o, d.path) && !dirs[fileName] {
			child = &Directory{c: d.c, cs: d.cs, so: &o, sh: swift.Headers{}, path: path, name: fileName}
			dirs[fileName] = true
			goto finish
		}

		// This is a pure swift object
		if !strings.HasSuffix(o.Name, "/") {
			child = &Object{path: path, name: fileName, c: d.c, cs: d.cs, so: &o, sh: swift.Headers{}, p: d}

			// If we are writing to this object at the moment
			// we don't want to update the cache with this.
			if ChangeCache.Exist(d.c.Name, path) {
				child = ChangeCache.Get(d.c.Name, path)
				goto export
			}

			// Large objects needs extra information
			if isLargeObject(&o) {
				DirectoryLister.AddTask(child, tasks)
				child = nil
				count++
			}
		}

	finish:
		// Always fetch extra info if asked
		if child != nil && ExtraAttr {
			DirectoryLister.AddTask(child, tasks)
			child = nil
			count++
		}

	export:
		// Add nodes not requiring extra info
		if child != nil {
			direntries = append(direntries, child.Export())
			children[child.Name()] = child
		}

	}

	// Wait for directory lister to finish
	if count > 0 {
		done := 0
		for task := range tasks {
			done++
			direntries = append(direntries, task.Export())
			children[task.Name()] = task
			if done == count {
				break
			}
		}
	}

	DirectoryCache.AddAll(d.c.Name, d.path, d, children)

	return direntries, nil
}

// Lookup gets a children node if its name matches the requested direntry name.
// If the cache is empty for the current directory, it will fill it and try to
// match the requested direnty after this operation.
// It returns ENOENT if not found.
func (d *Directory) Lookup(ctx context.Context, req *fuse.LookupRequest, resp *fuse.LookupResponse) (fs.Node, error) {
	if _, found := DirectoryCache.Peek(d.c.Name, d.path); !found {
		d.ReadDirAll(ctx)
	}

	// Find matching child
	if item := DirectoryCache.Get(d.c.Name, d.path, req.Name); item != nil {
		if n, ok := item.(*Directory); ok {
			return n, nil
		}
		if n, ok := item.(*Object); ok {
			return n, nil
		}
		if n, ok := item.(*Symlink); ok {
			return n, nil
		}
	}

	return nil, fuse.ENOENT
}

// Mkdir creates a new directory node within the current directory. It is represented
// by an empty object ending with a slash in the Swift container.
func (d *Directory) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (fs.Node, error) {
	var (
		absPath = d.path + req.Name
	)

	// Create the file in swift
	if err := SwiftConnection.ObjectPutBytes(d.c.Name, absPath, nil, DirContentType); err != nil {
		return nil, fuse.EIO
	}

	// Directory object
	node := &Directory{
		name: req.Name,
		path: absPath + "/",
		so: &swift.Object{
			Name:         absPath,
			ContentType:  DirContentType,
			LastModified: time.Now(),
		},
		sh: swift.Headers{},
		c:  d.c,
		cs: d.cs,
	}

	// Cache eviction
	DirectoryCache.Set(d.c.Name, d.path, req.Name, node)

	return node, nil
}

// Name gets the direntry name
func (d *Directory) Name() string {
	return d.name
}

// Remove deletes a direntry and relevant node. It is not supported on container
// nodes. It handles standard and segmented object deletion.
func (d *Directory) Remove(ctx context.Context, req *fuse.RemoveRequest) error {
	var (
		path = d.path + req.Name
		node = DirectoryCache.Get(d.c.Name, d.path, req.Name)
	)

	if directory, ok := node.(*Directory); ok {
		return d.removeDirectory(directory, req.Name)
	}
	if object, ok := node.(*Object); ok {
		return d.removeObject(object, req.Name, path)
	}

	return fuse.ENOTSUP
}

func (d *Directory) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	return nil
}

func (d *Directory) move(oldContainer, oldPath, oldName, newContainer, newPath, newName string) error {
	// Get the old node from the cache
	oldNode := DirectoryCache.Get(d.c.Name, d.path, oldName)

	if oldObject, ok := oldNode.(*Object); ok {
		// Move a manifest, not the aggregated result of its segments
		if oldObject.segmented {
			return d.moveManifest(oldContainer, oldPath, oldName, newContainer, newPath, newName, oldObject)
		}
		// Move a standard object
		if !oldObject.segmented {
			return d.moveObject(oldContainer, oldPath, oldName, newContainer, newPath, newName, oldObject)
		}
	}

	return fuse.ENOTSUP
}

func (d *Directory) moveObject(oldContainer, oldPath, oldName, newContainer, newPath, newName string, o *Object) error {
	err := SwiftConnection.ObjectMove(oldContainer, oldPath+oldName, newContainer, newPath+newName)
	if err != nil {
		return err
	}

	o.name = newName
	o.path = newPath + newName

	DirectoryCache.Delete(oldContainer, oldPath, oldName)
	DirectoryCache.Set(newContainer, newPath, newName, o)

	return nil
}

func (d *Directory) moveManifest(oldContainer, oldPath, oldName, newContainer, newPath, newName string, o *Object) error {
	_, err := SwiftConnection.ManifestCopy(oldContainer, oldPath+oldName, newContainer, newPath+newName, nil)
	if err != nil {
		return err
	}
	err = SwiftConnection.ObjectDelete(oldContainer, oldPath+oldName)
	if err != nil {
		return err
	}

	o.name = newName
	o.path = newPath + newName

	DirectoryCache.Delete(oldContainer, oldPath, oldName)
	DirectoryCache.Set(newContainer, newPath, newName, o)

	return nil
}

func (d *Directory) removeDirectory(directory *Directory, name string) error {
	SwiftConnection.ObjectDelete(directory.c.Name, directory.so.Name)
	if _, found := DirectoryCache.Peek(directory.c.Name, directory.path); found {
		DirectoryCache.DeleteAll(directory.c.Name, directory.path)
	}

	DirectoryCache.Delete(directory.c.Name, d.path, directory.name)

	return nil
}

func (d *Directory) removeObject(object *Object, name, path string) error {
	if object.segmented {
		_, h, err := SwiftConnection.Object(d.c.Name, path)
		if err != nil {
			return err
		}
		if !SegmentPathRegex.Match([]byte(h[ManifestHeader])) {
			return fmt.Errorf("Invalid segment path for manifest %s", name)
		}
		if err := deleteSegments(d.cs.Name, h[ManifestHeader]); err != nil {
			return err
		}
	}

	SwiftConnection.ObjectDelete(d.c.Name, path)
	DirectoryCache.Delete(d.c.Name, d.path, name)

	return nil
}

// Rename moves a node from its current directory node to a new directory node and updates
// the cache.
func (d *Directory) Rename(ctx context.Context, req *fuse.RenameRequest, newDir fs.Node) error {
	if t, ok := newDir.(*Directory); ok {
		return d.move(d.c.Name, d.path, req.OldName, t.c.Name, t.path, req.NewName)
	}
	return fuse.ENOTSUP
}

// Symlink creates a new symbolic link to the specified target in the current directory.
func (d *Directory) Symlink(ctx context.Context, req *fuse.SymlinkRequest) (fs.Node, error) {
	var (
		absPath = d.path + req.NewName
	)

	headers := map[string]string{ObjectSymlinkHeader: req.Target}

	// Create the file in swift
	w, err := SwiftConnection.ObjectCreate(d.c.Name, absPath, false, "", LinkContentType, headers)
	if err != nil {
		return nil, err
	}

	w.Close()

	link := &Symlink{
		c:    d.c,
		p:    d,
		name: req.NewName,
		path: absPath,
		sh:   headers,
		so: &swift.Object{
			ContentType: LinkContentType,
			Name:        absPath,
			Bytes:       0,
		},
	}

	DirectoryCache.Set(d.c.Name, d.path, req.NewName, link)

	return link, nil
}

var (
	_ Node             = (*Directory)(nil)
	_ fs.Node          = (*Directory)(nil)
	_ fs.NodeCreater   = (*Directory)(nil)
	_ fs.NodeRemover   = (*Directory)(nil)
	_ fs.NodeMkdirer   = (*Directory)(nil)
	_ fs.NodeRenamer   = (*Directory)(nil)
	_ fs.NodeSetattrer = (*Directory)(nil)
	_ fs.NodeSymlinker = (*Directory)(nil)
)
