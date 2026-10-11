package hostusb

import (
	"os"
	"path/filepath"
	"testing"
)

func TestList(t *testing.T) {
	sys := t.TempDir()

	// path → content; a path ending in / is a directory
	files := map[string]string{
		"usb3/bDeviceClass":                    "09", // root hub
		"3-3/bDeviceClass":                     "09", // hub
		"3-3/idVendor":                         "05e3",
		"3-3/idProduct":                        "0610",
		"3-7/bDeviceClass":                     "ef",
		"3-7/idVendor":                         "174f",
		"3-7/idProduct":                        "244c",
		"3-7/busnum":                           "3",
		"3-7/devnum":                           "3",
		"3-7/manufacturer":                     "Generic",
		"3-7/product":                          "Integrated Camera",
		"3-10/bDeviceClass":                    "e0",
		"3-10/idVendor":                        "8087",
		"3-10/idProduct":                       "0026",
		"3-10/busnum":                          "3",
		"3-10/devnum":                          "7",
		"2-2.2/bDeviceClass":                   "00",
		"2-2.2/idVendor":                       "0bda",
		"2-2.2/idProduct":                      "8153",
		"2-2.2/busnum":                         "2",
		"2-2.2/devnum":                         "5",
		"2-2.2/2-2.2:1.0/net/enx00e04c447098/": "",
		"3-3.3.2/bDeviceClass":                 "00",
		"3-3.3.2/idVendor":                     "05e3",
		"3-3.3.2/idProduct":                    "0751",
		"3-3.3.2/busnum":                       "3",
		"3-3.3.2/devnum":                       "15",
		"3-3.3.2/manufacturer":                 "USB Storage",
		"3-3.3.2/product":                      "USB Storage",
		"3-3.3.2/3-3.3.2:1.0/host1/target1:0:0/1:0:0:0/block/sda/": "",
		"3-4/bDeviceClass": "00",
		"3-4/idVendor":     "0781",
		"3-4/idProduct":    "5567",
		"3-4/busnum":       "3",
		"3-4/devnum":       "20",
		"3-4/3-4:1.0/host2/target2:0:0/2:0:0:0/block/sdb/": "",
		"3-7:1.0/bInterfaceClass":                          "0e", // an interface, not a device
	}
	for p, content := range files {
		full := filepath.Join(sys, p)
		if p[len(p)-1] == '/' {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	mounts := "/dev/nvme0n1p2 / ext4 rw 0 0\n/dev/sdb1 /media/stick vfat rw 0 0\n"

	devices, err := List(sys, mounts)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]Device{}
	for _, d := range devices {
		got[d.ID()] = d
	}

	if len(devices) != 5 {
		t.Fatalf("%d devices, want 5 (no hubs, no interfaces): %+v", len(devices), devices)
	}
	if d := got["174f:244c"]; d.Name != "Generic Integrated Camera" || d.InUse != "" || d.Bus != 3 || d.Address != 3 {
		t.Errorf("camera %+v", d)
	}
	if d := got["8087:0026"]; d.Name != "USB device 8087:0026" {
		t.Errorf("unnamed device %+v", d)
	}
	if d := got["0bda:8153"]; d.InUse != "network card enx00e04c447098" {
		t.Errorf("network card %+v", d)
	}
	if d := got["05e3:0751"]; d.InUse != "" || d.Name != "USB Storage" {
		t.Errorf("card reader with nothing mounted %+v", d)
	}
	if d := got["0781:5567"]; d.InUse != "disk sdb, mounted" {
		t.Errorf("mounted stick %+v", d)
	}
	if devices[0].Bus != 2 {
		t.Errorf("not sorted by bus: %+v", devices[0])
	}
}
