package roots

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const mountinfo = `21 1 252:0 / / rw,relatime shared:1 - ext4 /dev/mapper/vg-root rw
38 21 259:0 / /mnt/data rw,relatime shared:85 - ext4 /dev/nvme0n1 rw
39 21 0:40 / /mnt/tmp rw - tmpfs tmpfs rw
40 21 8:17 / /mnt/usb\040drive rw - vfat /dev/sdb1 rw
41 21 259:0 / /mnt/data-again rw - ext4 /dev/nvme0n1 rw
42 21 0:50 / /mnt/nas rw - nfs4 nas:/export rw
43 21 8:33 / /media/other rw - ext4 /dev/sdc1 rw
44 21 0:60 / /mnt rw - ext4 /dev/sdd1 rw
2639 2472 259:0 / /hostfs/mnt/data ro,relatime master:85 - ext4 /dev/nvme0n1 rw
`

func TestMountsUnder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mountinfo")
	os.WriteFile(path, []byte(mountinfo), 0o644)

	got := mountsUnder("/mnt", path)
	// tmpfs is skipped, the second bind of the same source is skipped, and
	// the escaped space is decoded.
	want := []string{"/mnt", "/mnt/data", "/mnt/nas", "/mnt/usb drive"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mountsUnder(/mnt) = %q, want %q", got, want)
	}
	if got := mountsUnder("/hostfs/mnt", path); !reflect.DeepEqual(got, []string{"/hostfs/mnt/data"}) {
		t.Errorf("inside a container: %q", got)
	}
}

func TestResolverInContainer(t *testing.T) {
	host := t.TempDir()
	os.MkdirAll(filepath.Join(host, "mnt", "kanshi-test"), 0o755)
	os.MkdirAll(filepath.Join(host, "srv"), 0o755)

	r := NewResolver([]string{"/=" + host, "/mnt/kanshi-test", "Media=" + host + "/srv"}, host)
	got := r.Roots()
	want := []Root{
		{Label: "/", Path: host},
		// A host path that does not exist in the container is found under
		// the host root, and labelled as the host path.
		{Label: "/mnt/kanshi-test", Path: host + "/mnt/kanshi-test"},
		{Label: "Media", Path: host + "/srv"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("roots = %+v\nwant   %+v", got, want)
	}
	if p := r.ContainerPath("/var/lib/docker"); p != host+"/var/lib/docker" {
		t.Errorf("ContainerPath = %q", p)
	}
	if p := r.HostPath(host + "/mnt/data"); p != "/mnt/data" {
		t.Errorf("HostPath = %q", p)
	}
}

func TestResolverAutoIncludesRoot(t *testing.T) {
	got := NewResolver([]string{"auto"}, "").Roots()
	if len(got) == 0 || got[0] != (Root{Label: "/", Path: "/"}) {
		t.Errorf("auto roots start with %+v", got)
	}
}
