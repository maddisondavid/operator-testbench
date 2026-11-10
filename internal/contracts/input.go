package contracts

// InputFile is an immutable, contained snapshot supplied by Workspace.
type InputFile struct {
	Name string
	Dir  bool
	Mode int64
	Data []byte
}

// OutputSink operations must respect cancellation; implementations support only
// bounded local regular-file storage, not FIFO/device/network filesystem sinks.

// FileEntry is a contained directory-listing value, not a filesystem handle.
type FileEntry struct {
	Name      string
	Directory bool
	Regular   bool
}
