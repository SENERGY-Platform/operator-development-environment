/*
 * Copyright 2026 InfAI (CC SES)
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package plaincode_test

import (
	"strings"
	"testing"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/plaincode"
)

// The subset exists to stop a developer being asked forty times about the same
// kind of cell. These are what that kind of cell looks like.
func TestTheDullSubsetIsRecognised(t *testing.T) {
	for _, code := range []string{
		"df.head()",
		"df.describe()",
		"print(df.shape)",
		"df.columns",
		"readings.dtypes",
		"x = df[\"power\"].mean()",
		"print(len(df))",
		"df[df[\"power\"] > 100].shape",
		"df.groupby(\"device\").size",
		"sorted(df.columns)",
		"[c for c in df.columns if c.startswith(\"p\")]",
		"print(df.head(), df.dtypes)",
		"# how much is there?\ndf.shape",
		"total = df[\"power\"].sum()\nprint(total)",
		"df.index.min(), df.index.max()",
	} {
		if ok, why := plaincode.Recognised(code); !ok {
			t.Errorf("not recognised: %q\n  because: %s", code, why)
		}
	}
}

/*
Everything below has to be asked about, and the reasons differ.

Some of it is dangerous, some is merely outside the vocabulary, and the package
does not distinguish those — it recognises or it does not. Both belong here,
because both must produce a prompt: a recogniser that let the second kind through
on the grounds that it looked harmless would be making the safety judgement this
package refuses to make.
*/
func TestAnythingElseIsNotRecognised(t *testing.T) {
	cases := map[string]string{
		// reaching out of the process
		"import os":                         "an import",
		"from os import system":             "a from-import",
		"__import__(\"os\").system(\"ls\")": "a dunder import",
		"open(\"/etc/passwd\").read()":      "opening a file",
		"eval(\"1+1\")":                     "eval",
		"exec(\"x=1\")":                     "exec",
		"compile(\"x\", \"f\", \"eval\")":   "compile",
		"globals()":                         "globals",
		"getattr(df, \"to_csv\")":           "getattr",
		"os.system(\"ls\")":                 "a call through os",
		"!ls -la":                           "a shell escape",
		"%timeit df.sum()":                  "a magic",
		"%%bash\nls":                        "a cell magic",
		// the obfuscation a denylist would miss, caught because the *names* are
		// unknown rather than because the strings were read
		"getattr(__builtins__, \"ev\" + \"al\")(\"1\")": "a built name",
		"f\"{__import__('os').system('ls')}\"":          "an f-string",
		// writing, which is not inspection
		"df.to_csv(\"out.csv\")":    "writing a file",
		"model.save(\"model.pkl\")": "saving",
		// shape that would let two statements wear the shape of one
		"x = 1; import os": "two statements on a line",
		"x = \\\n  1":      "a line continuation",
		// defining rather than inspecting
		"def f():\n    return 1": "a definition",
		"lambda x: x":            "a lambda",
		"class C:\n    pass":     "a class",
		// nothing at all
		"":    "empty",
		"   ": "blank",
	}
	for code, what := range cases {
		ok, why := plaincode.Recognised(code)
		if ok {
			t.Errorf("recognised %s, which must be asked about: %q", what, code)
			continue
		}
		if strings.TrimSpace(why) == "" {
			t.Errorf("refused %q without saying why", code)
		}
	}
}

// A string is data. Its contents must not make a cell unrecognised, and must not
// make one recognised either — the word has to be blanked, not skipped.
func TestStringContentsAreNeitherReadNorTrusted(t *testing.T) {
	if ok, why := plaincode.Recognised(`print("import os")`); !ok {
		t.Errorf("a keyword inside a string made the cell unrecognised: %s", why)
	}
	if ok, _ := plaincode.Recognised(`eval("df.head()")`); ok {
		t.Error("a dull-looking string argument made eval recognisable")
	}
	// The escape has to be honoured, or the scan resumes inside the literal and
	// reads its tail as code.
	if ok, why := plaincode.Recognised(`print("a \" quote")`); !ok {
		t.Errorf("an escaped quote ended the literal early: %s", why)
	}
	if ok, _ := plaincode.Recognised(`print("""` + "\n" + `import os` + "\n" + `""")`); !ok {
		t.Error("a triple-quoted literal was not read as one")
	}
}

// The reason is shown to the developer, so it has to name the thing that stopped
// it rather than say "no".
func TestTheReasonNamesWhatStoppedIt(t *testing.T) {
	for code, want := range map[string]string{
		"import os":          "import",
		"df.to_csv(\"a\")":   "to_csv",
		"x = 1; y = 2":       "one line",
		"getattr(df, \"x\")": "getattr",
	} {
		ok, why := plaincode.Recognised(code)
		if ok {
			t.Fatalf("recognised %q", code)
		}
		if !strings.Contains(why, want) {
			t.Errorf("reason for %q was %q, wanted it to mention %q", code, why, want)
		}
	}
}
