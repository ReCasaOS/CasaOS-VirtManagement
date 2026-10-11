package libvirt

import "testing"

func TestFormatVersion(t *testing.T) {
	if got := FormatVersion(12000000); got != "12.0.0" {
		t.Errorf("FormatVersion(12000000) = %q", got)
	}

	if got := FormatVersion(10002001); got != "10.2.1" {
		t.Errorf("FormatVersion(10002001) = %q", got)
	}
}

func TestSupportsUEFI(t *testing.T) {
	cases := []struct {
		name string
		xml  string
		want bool
	}{
		{
			name: "efi listed",
			xml: `<domainCapabilities><os supported='yes'>
				<enum name='firmware'><value>bios</value><value>efi</value></enum>
				<loader supported='yes'/></os></domainCapabilities>`,
			want: true,
		},
		{
			name: "bios only",
			xml:  `<domainCapabilities><os supported='yes'><enum name='firmware'><value>bios</value></enum></os></domainCapabilities>`,
		},
		{
			name: "os not supported",
			xml:  `<domainCapabilities><os supported='no'><enum name='firmware'><value>efi</value></enum></os></domainCapabilities>`,
		},
		{
			name: "no firmware enum",
			xml:  `<domainCapabilities><os supported='yes'/></domainCapabilities>`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SupportsUEFI(tc.xml)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
