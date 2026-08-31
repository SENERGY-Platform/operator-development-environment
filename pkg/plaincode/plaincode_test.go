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
Converting a frame is reading it, and the `to_*` family is split by that.

The writers are refused elsewhere in this file and stay refused — `to_csv` and
`to_sql` put the data somewhere, which is a different act and a fair thing to be
asked about. These produce a value and leave the object alone. They were absent
because they share a prefix with the writers, not because anything distinguished
them, and the first line of the cell that reaches everything else in the
vocabulary is a `to_pandas`.
*/
func TestConvertingAFrameIsReadingIt(t *testing.T) {
	for _, code := range []string{
		"df = frame.to_pandas()",
		"print(df.head(3).to_string())",
		`print("nulls:", df.isna().sum().to_dict())`,
		"df[\"power\"].to_numpy()",
		"df[\"power\"].to_frame()",
		"list(df.columns.to_list())",
		// The cell this was found from, whole.
		`df = frame.to_pandas()
print("rows", len(df), "| columns", list(df.columns))
print(df.head(3).to_string())
print("nulls:", df.isna().sum().to_dict())
print("time dtype:", df["time"].dtype, "| span", df["time"].min(), "->", df["time"].max())`,
	} {
		if ok, why := plaincode.Recognised(code); !ok {
			t.Errorf("not recognised: %q\n  because: %s", code, why)
		}
	}

	// The half that writes is not admitted by the half that does not.
	for code, act := range map[string]string{
		`df.to_csv("out.csv")`:     "writing a file",
		`df.to_sql("t", conn)`:     "writing a table",
		`df.to_parquet("out.pq")`:  "writing a file",
		`df.to_pickle("out.pkl")`:  "writing a file",
		`df.to_json("out.json")`:   "writing a file",
		`df.to_feather("out.f")`:   "writing a file",
		`df.to_excel("out.xlsx")`:  "writing a file",
		`df.to_hdf("out.h5", "k")`: "writing a file",
		`df.to_clipboard()`:        "leaving the pod",
		`frame.write_parquet("p")`: "writing a file",
	} {
		if ok, _ := plaincode.Recognised(code); ok {
			t.Errorf("recognised %q, which is %s", code, act)
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
		//
		// `import os` is recognised now, and `os.system` is what is refused — see
		// TestReadingIsInspectionAndWritingIsNot. What stays here is the import form
		// that binds a bare name the variable hole would wave through.
		"from os import system":             "a from-import",
		"__import__(\"os\").system(\"ls\")": "a dunder import",
		"open(\"out.csv\", \"w\")":          "opening a file to write",
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
		"f\"{__import__('os').system('ls')}\"":          "a dunder inside an f-string field",
		// writing, which is not inspection
		"df.to_csv(\"out.csv\")":    "writing a file",
		"model.save(\"model.pkl\")": "saving",
		// a semicolon launders nothing: both statements are scanned
		"x = 1; os.system(\"ls\")": "a call through os behind a semicolon",
		"x = \\\n  1":              "a line continuation",
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

/*
Reading is inspection; writing is not.

The cell that reads a file out of the checkout and prints part of it is the same
kind of thing as `df.head()` — a developer looking at their own work in their own
pod — and it was being asked about because `open` was absent from the vocabulary
and `os` was refused outright. What separates it from writing is the mode, and
what separates it from a credential leaving the pod is a short list of paths.

The refusals below are the load-bearing half. Every one of them is refused by the
vocabulary rather than by a rule about danger: `os.system` because `system` is not
an attribute this package knows, `from os import system` because that form binds a
bare name the variable hole would admit, `open(p, mode)` because a mode this
cannot read is one it will not claim to have read.
*/
func TestReadingIsInspectionAndWritingIsNot(t *testing.T) {
	recognised := []string{
		// The cell this was written for.
		"src = open(os.path.join(WS, 'cycles.py')).read()\n" +
			"i = src.index('def state_window')\n" +
			"print(src[i:i+5200])",
		`print(open("cycles.py").read())`,
		`print(open(path, "rb").read())`,
		// An import binds a name whose every use is still gated.
		"import numpy as np\nprint(np.mean(xs))",
		"import os\nprint(os.path.exists(p))",
		// A semicolon is two statements, both of them scanned.
		"total = df.size; print(total)",
		// A constant the developer's own code bound.
		"print(os.path.join(WS, 'cycles.py'))",
		`print(pathlib.Path("cycles.py").read_text()[:200])`,
	}
	for _, code := range recognised {
		if ok, why := plaincode.Recognised(code); !ok {
			t.Errorf("not recognised: %q\n  because: %s", code, why)
		}
	}

	refused := map[string]string{
		`open("out.csv", "w").write("x")`:                "writing",
		`open("out.csv", "a")`:                           "appending",
		`open("f", "r+")`:                                "a read handle that can write",
		`open(p, mode)`:                                  "a mode this check cannot read",
		`f = open`:                                       "the builtin as a value",
		`os.system("ls")`:                                "a call through os",
		`os.remove(p)`:                                   "removing a file",
		`print(os.environ)`:                              "the environment",
		`print(os.getenv("JUPYTERHUB_API_TOKEN"))`:       "an environment read",
		`from pathlib import Path`:                       "a from-import",
		`print(open("/home/jovyan/.ssh/id_rsa").read())`: "an SSH key",
		`print(open("/var/run/secrets/kubernetes.io/serviceaccount/token").read())`: "a service account token",
		`print(pathlib.Path(p).write_text("x"))`:                                    "writing through pathlib",
		`shutil.rmtree(p)`:                                                          "removing a tree",
		`print(SomeClass)`:                                                          "a CamelCase name",
	}
	for code, what := range refused {
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

/*
Grepping a file that was read is still reading it.

These shapes come from the confirmations one developer actually answered — 241
run_code cells over four days, of which they approved 232. Nine were recognised.
The ones below were refused for a member the vocabulary had not been given yet:
`re.search` and `.group` over source the cell had just read, `joinpath` where the
`/` operator would have passed, `np.array` and `.tolist` on values already in the
kernel, `json.dumps` of a dict the cell built. None of them does anything the
`.find` and `read_text` already in the list do not.

The refusals are the half that had to keep holding, and they are why the widening
is a list of members rather than a rule about lower-case names: `write_text`,
`savez_compressed` and `rglob` are spelled exactly like the members above and are
not in it. Enumerating a tree is kept out with them — a cell that names the file
it reads has been told what to look at, and one that walks the pod has not.
*/
func TestGreppingWhatWasReadIsStillReading(t *testing.T) {
	recognised := []string{
		// The cell this was written for, from 2026-08-31.
		"import pathlib, re\n" +
			"p = pathlib.Path(WS)\n" +
			"src = (p/\"training.py\").read_text()\n" +
			"m = re.search(r\"def train_model\\(\", src, re.S|re.M)\n" +
			"print(src[m.start():m.start()+400])",
		`print(re.findall(r"log_metric\(", src))`,
		`print([m.group(0) for m in re.finditer(r"^def ", src, re.M)])`,
		`print(LIB.joinpath("util/op_ml.py").read_text()[:400])`,
		`print(np.array(xs).mean(), np.array(xs).tolist())`,
		`print(json.dumps(d["data"], indent=1)[:500])`,
		"rows = []\nrows.append(len(src))\nrows.sort()\nprint(rows)",
		`print(hashlib.sha256(src.encode()).hexdigest())`,
		`print(os.path.getmtime(p), os.path.getsize(p))`,
	}
	for _, code := range recognised {
		if ok, why := plaincode.Recognised(code); !ok {
			t.Errorf("not recognised: %q\n  because: %s", code, why)
		}
	}

	refused := map[string]string{
		`p.write_text(src)`:                    "writing through pathlib",
		`np.savez_compressed("out.npz", a=xs)`: "persisting an array",
		`df.to_csv("out.csv")`:                 "persisting a frame",
		`print(list(p.rglob("*.py")))`:         "enumerating a tree",
		`print(list(p.glob("*.py")))`:          "enumerating a directory",
		`print(list(p.iterdir()))`:             "listing a directory",
		`p.unlink()`:                           "removing a file",
		`p.mkdir()`:                            "creating a directory",
		`print(re.sub(r"a", "b", src))`:        "a member that was not added",
	}
	for code, what := range refused {
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

/*
An f-string is as dull as what it interpolates.

The fields are put back into the scan and the text between them stays data, so
`print(f"{df.shape}")` is judged exactly as `print(df.shape)` is — which is the
point: two thirds of the confirmations auto mode was meant to spare a developer
were f-strings, refused for the quotes rather than for anything inside them.

The refusals below are the other half of that. Everything this scan cannot follow
is refused rather than half-read, and the one that matters most is a nested
f-string: blanking it as an ordinary literal would hide a field that Python does
evaluate.
*/
func TestAnFStringIsReadThroughItsFields(t *testing.T) {
	recognised := []string{
		`print(f"rows: {df.shape[0]}")`,
		`print(f"{df.shape}")`,
		// The format spec and the conversion are applied to the value, not evaluated.
		`print(f"{df.size:.2f}")`,
		`print(f"{df.columns!r}")`,
		// The debug form, which is a name and an equals sign.
		`print(f"{df.empty=}")`,
		// Escaped braces are text.
		`print(f"{{literal}} {df.ndim}")`,
		// A literal inside a field, quoted the other way.
		`print(f"{df.loc['a']}")`,
		// A comparison inside a field, whose `!` is not a shell escape.
		`print(f"{df.size != 0}")`,
		// A dict literal inside a field: its brace does not end the field and its
		// colon is not a format spec.
		`print(f"{ {'a': df.ndim}['a'] }")`,
		// Triple-quoted, over two lines.
		"print(f\"\"\"{df.ndim}\n{df.size}\"\"\")",
	}
	for _, code := range recognised {
		if ok, why := plaincode.Recognised(code); !ok {
			t.Errorf("not recognised: %q — %s", code, why)
		}
	}

	refused := map[string]string{
		// The field is scanned, so what is in it decides.
		`print(f"{open('out.txt', 'w')}")`: "opening a file to write in a field",
		`print(f"{eval('1+1')}")`:          "eval in a field",
		`print(f"{df.to_csv('out.csv')}")`: "writing from a field",
		// A nested f-string is evaluated; blanking it would hide that.
		`print(f"{f'{eval(chr(1))}'}")`: "a nested f-string",
		// Syntax this scan does not follow.
		`print(f"{df.loc["a"]}")`:     "a field in the string's own quote",
		`print(f"{df.ndim:{width}}")`: "a field inside a format spec",
		`print(f"{df.ndim")`:          "an unterminated field",
		`print(f"unclosed {df.ndim}`:  "an unterminated literal",
	}
	for code, what := range refused {
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
		"os.environ":         "environ",
		"df.to_csv(\"a\")":   "to_csv",
		"open(p, mode)":      "mode",
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
