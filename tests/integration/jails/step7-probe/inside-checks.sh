J=skep-$1
echo "== jls -n (the kernel's parameters)"; jls -n -j $J | tr ' ' '\n' | grep -E '^(jid|name|path|host.hostname|vnet|persist|enforce_statfs|children.max|securelevel|devfs_ruleset|sysvshm|sysvsem|sysvmsg)=|^allow\.' | grep -v '^allow\.no' ; echo "(every allow.* not listed above is off: jls prints it as allow.noX)"
echo "== inside: kernel view"; jexec -l $J sh -c 'echo kern.securelevel=$(sysctl -n kern.securelevel) security.jail.jailed=$(sysctl -n security.jail.jailed) security.jail.vnet=$(sysctl -n security.jail.vnet); sysctl security.jail.param.allow 2>/dev/null | head -0; for p in suser reserved_ports raw_sockets set_hostname mlock mount chflags sysvipc; do echo "security.jail.$p-ish: $(sysctl -n security.jail.${p}_allowed 2>/dev/null || echo n/a)"; done' 
echo "== inside: processes"; ps -J $J -o user,command
echo "== inside: devices"; ls $(jls -j $J path)/dev | tr '\n' ' '; echo
echo "== inside: interfaces"; jexec -l $J ifconfig -l
echo "== bind a port below 1024 (tcp/22) as the jail's root"
jexec -l $J sh -c 'nc -l 127.0.0.1 22 & p=$!; sleep 1; sockstat -l -4 | grep ":22 " || echo NOT LISTENING; kill $p'
echo "== raw socket (recorded, not expected to fail: amendment f)"; jexec -l $J ping -c1 127.0.0.1 2>&1 | sed -n '1,2p'; echo "ping exit: $(jexec -l $J ping -c1 -q 127.0.0.1 >/dev/null 2>&1; echo $?)"
echo "== refused"
jexec -l $J sh -c 'echo -n "mount tmpfs: "; mount -t tmpfs tmpfs /mnt 2>&1; echo -n "kldload: "; kldload if_epair 2>&1; echo -n "sysctl kern.hostname=x: "; sysctl kern.hostname=x 2>&1 | tail -1; echo -n "hostname x: "; hostname x 2>&1; echo -n "chflags noschg /bin/sh: "; chflags noschg /bin/sh 2>&1 && echo "(no error)"; echo -n "securelevel lower: "; sysctl kern.securelevel=0 2>&1 | tail -1'
