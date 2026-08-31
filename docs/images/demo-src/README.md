# Source for `docs/images/demo.gif`

Four still frames, each one real output from the command shown in it, held
long enough to read. There is no typing animation on purpose: the point of the
demo is the argument the four screens make together, and a typewriter effect
would triple the file size to add nothing.

`mk.py` writes the HTML; the numbers come from text captured by actually
running each command, so they can be regenerated and checked rather than
edited by hand.

## Regenerating

Capture the output, one file per scene:

```bash
pyrite run --example golden-cross --from 2018-01-02 --to 2023-12-29 \
  | sed -n '/^Golden cross/,$p'            > f1.txt
pyrite run --example golden-cross --from 2018-01-02 --to 2023-12-29 \
  | sed -n '/believe this/,/Comparison/p'  > s1b.txt
pyrite sweep --example golden-cross --from 2015-01-05 --to 2023-12-29 \
  | sed -n '/How much of this is real/,$p' > s2.txt
pyrite walkforward --example golden-cross --from 2010-01-05 --to 2023-12-29 \
  --train 500 --test 150 | sed -n '/Mean in-sample/,$p' > s3.txt
```

Then render and assemble. Firefox needs a throwaway profile per frame, or it
refuses to start when a browser is already running:

```bash
python3 mk.py
for i in 1 2 3 4; do
  mkdir -p p$i
  firefox --headless --profile "$PWD/p$i" --no-remote \
    --screenshot "$PWD/f$i.png" --window-size=960,540 "file://$PWD/f$i.html"
done
ffmpeg -y -f concat -safe 0 -i list.txt \
  -vf "fps=2,scale=920:-1:flags=lanczos,split[a][b];\
[a]palettegen=max_colors=64:stats_mode=diff[p];\
[b][p]paletteuse=dither=none:diff_mode=rectangle" \
  -loop 0 demo.gif
```

Two frames per second is deliberate: the scenes are static, so a higher rate
only multiplies identical frames.
