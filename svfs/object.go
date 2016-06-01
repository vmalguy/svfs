package svfs

import (
	"os"
	"strconv"
	"sync"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"github.com/xlucas/swift"
	"golang.org/x/net/context"
)

const (
	ManifestHeader      = "X-Object-Manifest"
	ObjectMetaHeader    = "X-Object-Meta-"
	ObjectSymlinkHeader = ObjectMetaHeader + "Symlink-Target"
	ObjectMtimeHeader   = ObjectMetaHeader + "Mtime"
	ObjectSizeHeader    = ObjectMetaHeader + "Crypto-Origin-Size"
	ObjectNonceHeader   = ObjectMetaHeader + "Crypto-Nonce"
)

// Object is a node representing a swift object.
// It belongs to a container and segmented objects
// are bound to a container of segments.
type Object struct {
	name      string
	path      string
	so        *swift.Object
	sh        swift.Headers
	c         *swift.Container
	cs        *swift.Container
	p         *Directory
	m         sync.Mutex
	segmented bool
	writing   bool
}

// Attr fills the file attributes for an object node.
func (o *Object) Attr(ctx context.Context, a *fuse.Attr) (err error) {
	a.Size = o.size()
	a.BlockSize = uint32(BlockSize)
	a.Blocks = (a.Size / uint64(a.BlockSize)) * 8
	a.Mode = os.FileMode(DefaultMode)
	a.Gid = uint32(DefaultGID)
	a.Uid = uint32(DefaultUID)
	a.Mtime = getMtime(o.so, o.sh)
	a.Ctime = a.Mtime
	a.Crtime = a.Mtime
	return nil
}

// Export converts this object node as a direntry.
func (o *Object) Export() fuse.Dirent {
	return fuse.Dirent{
		Name: o.Name(),
		Type: fuse.DT_File,
	}
}

func (o *Object) open(mode fuse.OpenFlags, flags *fuse.OpenResponseFlags) (oh *ObjectHandle, err error) {
	oh = &ObjectHandle{
		target: o,
		create: mode&fuse.OpenCreate == fuse.OpenCreate,
	}

	// Append mode is not supported
	if mode&fuse.OpenAppend == fuse.OpenAppend {
		return nil, fuse.ENOTSUP
	}

	if mode.IsReadOnly() {
		return oh, nil
	}
	if mode.IsWriteOnly() {
		o.m.Lock()
		ChangeCache.Add(o.c.Name, o.path, o)

		// Can't write with an offset
		*flags |= fuse.OpenNonSeekable
		// Don't cache writes
		*flags |= fuse.OpenDirectIO

		// Remove segments
		if o.segmented && oh.create {
			if err = o.removeSegments(); err != nil {
				return oh, err
			}
		}

		// Create new object
		if oh.create {
			oh.wd, err = newWriter(oh.target.c.Name, oh.target.path, &oh.nonce)
		}

		return oh, err
	}

	return nil, fuse.ENOTSUP
}

// Open returns the file handle associated with this object node.
func (o *Object) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	return o.open(req.Flags, &resp.Flags)
}

func (o *Object) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	// Change file size. May be used by the kernel
	// to truncate files to 0 size instead of opening
	// them with O_TRUNC flag.
	if req.Valid.Size() {
		o.so.Bytes = int64(req.Size)
		if req.Size == 0 && o.segmented {
			return o.removeSegments()
		}
		return nil
	}

	if !ExtraAttr || !req.Valid.Mtime() {
		return fuse.ENOTSUP
	}

	// Change mtime
	if !req.Mtime.Equal(getMtime(o.so, o.sh)) {
		if o.writing {
			o.m.Lock()
			defer o.m.Unlock()
		}
		h := o.sh.ObjectMetadata().Headers(ObjectMetaHeader)
		o.sh[ObjectMtimeHeader] = swift.TimeToFloatString(req.Mtime)
		h[ObjectMtimeHeader] = o.sh[ObjectMtimeHeader]
		return SwiftConnection.ObjectUpdate(o.c.Name, o.so.Name, h)
	}

	return nil
}

// Name gets the name of the underlying swift object.
func (o *Object) Name() string {
	return o.name
}

func (o *Object) removeSegments() error {
	o.segmented = false
	if err := deleteSegments(o.cs.Name, o.sh[ManifestHeader]); err != nil {
		return err
	}
	delete(o.sh, ManifestHeader)
	return nil
}

func (o *Object) size() uint64 {
	if Encryption && o.sh[ObjectSizeHeader] != "" {
		size, _ := strconv.ParseInt(o.sh[ObjectSizeHeader], 10, 64)
		return uint64(size)
	}
	return uint64(o.so.Bytes)
}

var (
	_ Node             = (*Object)(nil)
	_ fs.Node          = (*Object)(nil)
	_ fs.NodeSetattrer = (*Object)(nil)
	_ fs.NodeOpener    = (*Object)(nil)
)
