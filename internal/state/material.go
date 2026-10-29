package state

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

const (
	materialFolder = "tls"
	materialLimit  = 64 << 10
)

// ErrInvalidMaterialName marks a material name that is not a single plain file name.
var ErrInvalidMaterialName = errors.New("state: invalid material name")

func materialName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("%w: %q", ErrInvalidMaterialName, name)
	}
	return nil
}

// ReadMaterial reads one public certificate file of the environment from its tls folder. ok is false
// when the file does not exist yet. The name is a single file name; the folder is opened through the
// environment's os.Root, so no name can leave it.
func (e *Env) ReadMaterial(name string) (data []byte, ok bool, err error) {
	if err := materialName(name); err != nil {
		return nil, false, err
	}
	dir, err := e.sub(materialFolder)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer dir.Close()
	data, err = e.read(dir, name, materialLimit)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// WriteMaterial writes one public certificate file into the tls folder, creating the folder with mode
// 0700 first. It writes through a temporary file and a rename, like every other file of the
// environment. Only public material belongs here: a private key never does.
func (e *Env) WriteMaterial(name string, data []byte) error {
	if err := materialName(name); err != nil {
		return err
	}
	if err := e.root.Mkdir(materialFolder, dirMode); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("state: create %s: %w", materialFolder, err)
	}
	dir, err := e.sub(materialFolder)
	if err != nil {
		return err
	}
	defer dir.Close()
	return e.write(dir, name, data)
}
