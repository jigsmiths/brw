package services

import (
	"strings"
	"testing"
)

const plistDoc = "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
	"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
	"<plist version=\"1.0\">\n"

func TestApplyUserNameReserialisesLikeRubysToPlist(t *testing.T) {
	in := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<plist version=\"1.0\">\n" +
		"<dict>\n" +
		"  <key>Zeta</key><real>0.00001</real>\n" +
		"  <key>Label</key>\n  <string>a &amp; b &lt;c&gt; &quot;d&quot; &#39;e&#39; &#x41;</string>\n" +
		"  <key>Args</key><array><string>/bin/x</string><string/><integer>007</integer></array>\n" +
		"  <key>Empty</key><dict/>\n" +
		"  <key>NoArgs</key><array></array>\n" +
		"  <key>Flags</key><dict><key>A</key><true/><key>B</key><false/></dict>\n" +
		"  <key>When</key><date>2024-05-06T07:08:09+02:00</date>\n" +
		"  <key>Blob</key><data>aGVsbG8=</data>\n" +
		"  <key>Big</key><real>12345678901234567890.5</real>\n" +
		"</dict>\n</plist>\n"
	got, err := ApplyUserName([]byte(in), "svc")
	if err != nil {
		t.Fatal(err)
	}
	want := plistDoc +
		"<dict>\n" +
		"\t<key>Args</key>\n\t<array>\n\t\t<string>/bin/x</string>\n\t\t<string/>\n\t\t<integer>7</integer>\n\t</array>\n" +
		"\t<key>Big</key>\n\t<real>1.2345678901234567e+19</real>\n" +
		"\t<key>Blob</key>\n\t<data>\n\taGVsbG8=\n\t</data>\n" +
		"\t<key>Empty</key>\n\t<dict/>\n" +
		"\t<key>Flags</key>\n\t<dict>\n\t\t<key>A</key>\n\t\t<true/>\n\t\t<key>B</key>\n\t\t<false/>\n\t</dict>\n" +
		"\t<key>Label</key>\n\t<string>a &amp; b &lt;c&gt; &quot;d&quot; &#39;e&#39; A</string>\n" +
		"\t<key>NoArgs</key>\n\t<array/>\n" +
		"\t<key>UserName</key>\n\t<string>svc</string>\n" +
		"\t<key>When</key>\n\t<date>2024-05-06T07:08:09Z</date>\n" +
		"\t<key>Zeta</key>\n\t<real>1.0e-05</real>\n" +
		"</dict>\n</plist>\n"
	if string(got) != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestApplyUserNameReplacesAnExistingUser(t *testing.T) {
	first, err := ApplyUserName([]byte(plistDoc+"<dict>\n\t<key>Label</key>\n\t<string>x</string>\n</dict>\n</plist>\n"), "a")
	if err != nil {
		t.Fatal(err)
	}
	again, err := ApplyUserName(first, "b")
	if err != nil || string(again) != strings.Replace(string(first), ">a<", ">b<", 1) {
		t.Errorf("again = %s, %v", again, err)
	}
}

func TestApplyUserNameFailsWhereRubyDoes(t *testing.T) {
	for name, in := range map[string]string{
		"text":        "not a plist",
		"empty":       "",
		"array":       plistDoc + "<array/>\n</plist>\n",
		"unknown tag": plistDoc + "<dict><foo/></dict>\n</plist>\n",
	} {
		if got, err := ApplyUserName([]byte(in), "svc"); err == nil {
			t.Errorf("%s: want an error, got %s", name, got)
		}
	}
}
