git -C $1 remote add origin .
git -C $1 branch -M main
git -C $1 branch --set-upstream-to=origin/main main
