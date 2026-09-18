p = 'cmd/grounder/manifest_read.go'
s = open(p).read()
old = "\treturn r.s.AllEntries(ctx, limit)"
new = "\trows, _, total, err := r.s.AllEntries(ctx, limit)\n\treturn rows, len(rows), total, err // RED CONTROL: fabricate the draft count from the page"
assert s.count(old) == 1
open(p, 'w').write(s.replace(old, new))
print("RED-1 injected")
