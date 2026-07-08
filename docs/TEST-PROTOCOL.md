# Protocole de test — intégration Plakar KVM/DRBD (mode distant)

Objectif : valider le connecteur **importer `kvm`** en **mode distant** — Plakar
tourne dans la VM `plakar-server` et sauvegarde des VM situées sur l'hyperviseur
`FR-KVM-TEST1` **sans y installer Plakar**. Le contrôle passe par `qemu+ssh://`
et les disques sont lus/convertis à distance puis streamés par SSH.

> Version du plugin : **v0.1.0** (importer ; modes local **et** distant).
> À adapter si tu changes le numéro de version.

---

## 0. Topologie & conventions

```
+------------------+        SSH (contrôle virsh + données disque)        +-------------------+
|  plakar-server   |  ------------------------------------------------>  |   FR-KVM-TEST1    |
|  (VM dédiée)     |     qemu+ssh:// , ssh cat , qemu-img convert        |  hyperviseur KVM  |
|  - plakar        |                                                     |  + DRBD (Primary) |
|  - plugin kvm    |                                                     |  - VMs à sauver   |
|  - dépôt Kloset  |                                                     +-------------------+
+------------------+
```

Tags d'exécution :

| Tag | Où |
|-----|----|
| `[PLK]` | shell dans la VM **plakar-server** (là où on lance tout) |
| `[HV]` | shell sur l'**hyperviseur FR-KVM-TEST1** (vérifs / restauration) |
| `[GUEST]` | shell **dans la VM invitée à sauvegarder** (pour le guest-agent) |

Variables (à ajuster une fois) — `[PLK]` :

```bash
export HV=FR-KVM-TEST1                 # hyperviseur
export SSHUSER=root                    # user SSH autorisé à lire les disques (root pour DRBD)
export URI="qemu+ssh://$SSHUSER@$HV/system"
export REPO=/var/backups/kvm           # dépôt Kloset, LOCAL à plakar-server
export DOM=<nom-de-la-VM>              # domaine à sauvegarder (voir §5 pour le lister)
```

> ⚠️ **DRBD** : vise le nœud **Primary** — c'est le seul où `/dev/drbdN` est
> lisible.
>
> ⚠️ **Auto-sauvegarde** : si `DOM` est `plakar-server` elle-même (la VM qui
> exécute Plakar), utilise **`consistency=crash`**. Geler (`fsfreeze`) son propre
> système de fichiers racine pendant qu'on le lit peut bloquer le backup.

---

## 1. Prérequis

### 1.1 Sur plakar-server `[PLK]`

```bash
# client libvirt (pour que le plugin puisse lancer virsh -c qemu+ssh://...)
which virsh || sudo apt-get install -y libvirt-clients
which ssh
```

### 1.2 SSH non interactif vers l'hyperviseur `[PLK]`

```bash
# clé si besoin, puis copie sur l'hyperviseur
test -f ~/.ssh/id_ed25519 || ssh-keygen -t ed25519 -N '' -f ~/.ssh/id_ed25519
ssh-copy-id $SSHUSER@$HV

# TEST CLÉ : doit réussir SANS mot de passe et afficher les 2 versions
ssh -o BatchMode=yes $SSHUSER@$HV 'virsh version && qemu-img --version'
```

✅ Ce test doit passer sans invite de mot de passe. `qemu-img` doit être présent
sur l'hyperviseur (c'est là que la conversion tourne).

### 1.3 Contrôle libvirt distant `[PLK]`

```bash
virsh -c "$URI" list --all --name          # doit lister les domaines de l'HV
```

### 1.4 Guest-agent dans la VM cible (pour le mode `fsfreeze`) `[GUEST]`

```bash
# Debian/Ubuntu
sudo apt-get update && sudo apt-get install -y qemu-guest-agent
sudo systemctl start qemu-guest-agent
# RHEL/Alma/Rocky : sudo dnf install -y qemu-guest-agent && sudo systemctl start qemu-guest-agent
```

Validation depuis plakar-server `[PLK]` :

```bash
virsh -c "$URI" qemu-agent-command "$DOM" '{"execute":"guest-ping"}'   # -> {"return":{}}
virsh -c "$URI" domfsfreeze "$DOM" && virsh -c "$URI" domfsthaw "$DOM"  # doit réussir
```

> Si l'agent ne répond pas, ajoute le canal virtio à la VM (`[HV]` :
> `virsh edit "$DOM"`) puis redémarre-la :
> ```xml
> <channel type='unix'>
>   <target type='virtio' name='org.qemu.guest_agent.0'/>
> </channel>
> ```
> Sans agent, teste uniquement le mode `crash` (§7).

---

## 2. Build & packaging du plugin `[PLK]`

Nécessite Go ≥ 1.24 (si `make test` échoue dans la stdlib Go, voir §14 — réinstalle
Go proprement).

```bash
cd /opt/plakar-kvm
go mod tidy          # résout les dépendances (accès réseau requis)
make test            # tests unitaires (URI, parsing, local/ssh, shellQuote) -> PASS
make build           # produit ./kvmImporter

plakar pkg create manifest.yaml v0.1.0
ls -1 kvm_v0.1.0_*.ptar
```

---

## 3. Installation du plugin `[PLK]`

```bash
plakar pkg add ./kvm_v0.1.0_linux_amd64.ptar
plakar pkg show                      # doit lister kvm@v0.1.0
```

---

## 4. Création du dépôt Kloset (local à plakar-server) `[PLK]`

```bash
sudo mkdir -p "$REPO"
plakar at "$REPO" create             # définir une passphrase (à conserver !)
```

> Passphrase irrécupérable si perdue. Pour éviter la saisie pendant les tests :
> `export PLAKAR_PASSPHRASE='...'` (confirme le nom de variable avec `plakar help`).

---

## 5. Choisir la VM cible `[PLK]`

```bash
virsh -c "$URI" list --all --name          # choisis un domaine
export DOM=<nom-choisi>
virsh -c "$URI" domblklist "$DOM" --details   # note les disques (file/block, Source)
```

Repère si un disque est un device DRBD (`/dev/drbdN`, colonne Type = `block`).

---

## 6. Test A — smoke test : définitions seules (`include_disks=false`) `[PLK]`

But : valider la chaîne complète (connexion distante + export XML) sans toucher
aux disques.

```bash
plakar source add kvmDefsOnly kvm://$HV/system include_disks=false
plakar at "$REPO" backup @kvmDefsOnly
plakar at "$REPO" ls                        # note le SNAPID
```

Vérification :

```bash
SNAP=<SNAPID>
plakar at "$REPO" ls "$SNAP"                    # arborescence: <domaine>/domain.xml
plakar at "$REPO" cat "$SNAP:$DOM/domain.xml" | head -20
```

✅ Attendu : un `domain.xml` par domaine, identique à `virsh dumpxml`.

> Si l'utilisateur SSH n'est pas celui par défaut, ajoute `ssh_user=$SSHUSER`
> (et `ssh_port=...` si besoin), ou passe l'URI complète :
> `connect_uri=qemu+ssh://$SSHUSER@$HV/system`.

---

## 7. Test B — backup crash-consistent (disques inclus) `[PLK]`

But : sauvegarder disques + définitions, sans quiescing (rapide ; ne nécessite
pas le guest-agent). Les disques sont lus via `ssh cat`.

```bash
plakar source add kvmCrash kvm://$HV/system consistency=crash domains=$DOM
plakar at "$REPO" backup @kvmCrash
plakar at "$REPO" ls
```

Vérification :

```bash
SNAP=<SNAPID>
plakar at "$REPO" ls "$SNAP:$DOM/disks"        # une entrée par disque
plakar at "$REPO" check "$SNAP"                # intégrité (déchiffrement + checksums)
```

✅ Attendu : `<domaine>/disks/<nom-image>` présent, `check` = OK.

---

## 8. Test C — backup application-consistent par snapshot (`snapshot`) `[PLK]`

But : valider le mode **snapshot** — snapshot externe atomique (l'invité bascule
sur un overlay), lecture de la base figée streamée dans Plakar **sans copie de
staging**, puis `blockcommit --active --pivot` pour rapatrier l'overlay. C'est le
mode recommandé pour les gros disques (pas d'espace temporaire, gel de l'invité
réduit à l'instant du snapshot).

Prérequis : `overlay_dir` (défaut `/var/lib/libvirt/images`) doit être accessible
en écriture par QEMU sur l'hyperviseur.

Poser un marqueur horodaté dans l'invité juste avant `[GUEST]` :

```bash
date -u +%FT%TZ | sudo tee /root/plakar-marker.txt && sync
```

```bash
# [PLK]
plakar source add kvmSnap kvm://$HV/system consistency=snapshot domains=$DOM
plakar at "$REPO" backup @kvmSnap
plakar at "$REPO" ls
```

Observer sur l'hyperviseur pendant/après le backup `[HV]` :

```bash
# pendant : l'invité écrit sur un overlay ...-plakar-<ts>.qcow2
ls -l /var/lib/libvirt/images/${DOM}-*-plakar-*.qcow2 2>/dev/null
virsh domblklist "$DOM" --details       # la Source active = l'overlay

# après : l'overlay a été commité + supprimé, la Source revient à la base
ls -l /var/lib/libvirt/images/${DOM}-*-plakar-*.qcow2 2>/dev/null   # -> plus de fichier
virsh domblklist "$DOM" --details       # Source = disque d'origine
```

Vérification Plakar :

```bash
SNAP=<SNAPID>
plakar at "$REPO" ls "$SNAP:$DOM/disks"        # base d'origine (nom du disque source)
plakar at "$REPO" check "$SNAP"
```

✅ Attendu : `check` OK ; **après** le backup, plus aucun overlay `-plakar-*` et
`domblklist` repointe sur le disque d'origine (overlay bien rapatrié) ; la VM
n'a jamais été gelée plus que l'instant du snapshot.

> ⚠️ Si `check` KO **ou** qu'un overlay `-plakar-*` subsiste : le `blockcommit`
> a échoué — la VM tourne encore sur l'overlay. Rapatrie à la main :
> `virsh blockcommit "$DOM" <target> --active --pivot --wait` puis supprime
> l'overlay. Remonte-moi le cas.

### 8bis. Variante `fsfreeze` (copie complète) — optionnel

Alternative qui copie chaque disque via `qemu-img convert` (nécessite de
l'espace temporaire, gel plus long) :

```bash
plakar source add kvmFreeze kvm://$HV/system consistency=fsfreeze domains=$DOM
plakar at "$REPO" backup @kvmFreeze
plakar at "$REPO" ls "$SNAP:$DOM/disks"    # fichiers en .qcow2 (copies compactes)
```

---

## 9. Test D — restauration & validation réelle

> ⚠️ L'**exporter n'est pas encore implémenté** : la restauration extrait les
> fichiers (XML + images) dans un répertoire sur plakar-server, puis on redéfinit
> la VM à la main sur l'hyperviseur.

Extraction `[PLK]` :

```bash
mkdir -p /tmp/restore
plakar at "$REPO" restore -to /tmp/restore "$SNAP"
find /tmp/restore -maxdepth 3 -type f

# envoyer les fichiers vers l'hyperviseur
scp -r /tmp/restore/$DOM $SSHUSER@$HV:/tmp/
```

Redéfinition d'une VM de contrôle `[HV]` (⚠️ **ne pas écraser la VM d'origine** —
renomme domaine + chemins) :

```bash
cd /tmp/$DOM

# 1) placer le disque restauré
#    - image fichier :
cp disks/*.qcow2 /var/lib/libvirt/images/${DOM}-restored.qcow2
#    - disque DRBD raw à l'origine : réécrire dans un volume de test dédié
#      qemu-img convert -O raw disks/<img>.qcow2 /dev/<volume-de-test>

# 2) adapter le XML : nouveau nom + nouveau chemin de disque + retirer uuid/mac
cp domain.xml ${DOM}-restored.xml
sed -i "s#<name>$DOM</name>#<name>${DOM}-restored</name>#" ${DOM}-restored.xml
#    éditer <source .../> vers le disque restauré ; supprimer <uuid> et <mac>

# 3) définir et démarrer
virsh define ${DOM}-restored.xml
virsh start  ${DOM}-restored
```

Validation `[GUEST restaurée]` :

```bash
cat /root/plakar-marker.txt        # marqueur posé en §8 -> cohérence temporelle
```

✅ Attendu : la VM restaurée démarre, le marqueur est présent et cohérent.

---

## 10. Vérifications spécifiques DRBD `[HV]`

```bash
drbdadm status                       # ou: cat /proc/drbd  -> rôle Primary
virsh domblklist "$DOM" --details    # une Source de type block = /dev/drbdN ?
```

- Lancer un Test B/C `[PLK]` sur une VM dont un disque est `/dev/drbdN` et vérifier
  que `disks/drbdN` apparaît dans le snapshot.
- Vérifier que la réplication DRBD n'est pas perturbée (`drbdadm status` stable,
  pas de resync inattendu) pendant/après le backup.

✅ Attendu : disque DRBD sauvegardé (lu via SSH sur le Primary), réplication intacte.

---

## 11. Tests de robustesse / cas d'erreur `[PLK]`

| Cas | Condition | Attendu |
|-----|-----------|---------|
| VM éteinte | `virsh -c "$URI" shutdown $DOM` puis backup | lecture directe (crash forcé), backup OK |
| Filtre inconnu | `... domains=nexistepas` | aucun domaine sauvegardé, pas de crash |
| Sans guest-agent + fsfreeze | agent arrêté, `consistency=fsfreeze` | fallback crash + avertissement, backup OK |
| Snapshot sans guest-agent | agent arrêté, `consistency=snapshot` | snapshot crash-consistent (retry sans `--quiesce`), overlay commité, backup OK |
| overlay_dir non inscriptible | `overlay_dir=/root/nope` | échec propre du snapshot (erreur), pas d'overlay orphelin |
| Mauvaise location | `kvm:///bogus` | rejet à la config (schema/erreur claire) |
| SSH cassé | clé retirée / hôte injoignable | `Ping`/backup échoue proprement (pas de données tronquées) |
| Transfert interrompu | couper le réseau pendant un gros disque | erreur de lecture remontée (le snapshot ne valide pas un disque tronqué) |

---

## 12. Nettoyage

```bash
# [HV] VM de contrôle restaurée
virsh destroy ${DOM}-restored 2>/dev/null; virsh undefine ${DOM}-restored
rm -f /var/lib/libvirt/images/${DOM}-restored.qcow2 /tmp/$DOM/*.xml
rm -rf /tmp/$DOM

# [PLK]
rm -rf /tmp/restore
plakar source rm kvmDefsOnly kvmCrash kvmFreeze    # confirmer la sous-commande exacte
rm -rf "$REPO"                                      # ⚠️ supprime tous les snapshots de test
plakar pkg rm kvm                                   # confirmer la sous-commande exacte
```

---

## 13. Grille de résultats (à remplir)

| # | Test | Commande clé | Résultat attendu | OK/KO | Notes |
|---|------|--------------|------------------|-------|-------|
| 1 | Client virsh | `virsh --version` sur plakar-server | présent | | |
| 2 | SSH clé | `ssh -o BatchMode=yes $SSHUSER@$HV 'virsh version && qemu-img --version'` | sans mot de passe | | |
| 3 | Contrôle distant | `virsh -c "$URI" list --all` | liste des domaines | | |
| 4 | Guest agent | `virsh -c "$URI" domfsfreeze/thaw $DOM` | succès | | |
| 5 | Build + tests | `make test && make build` | PASS + binaire | | |
| 6 | Packaging + install | `plakar pkg create` / `add` / `show` | `kvm@v0.1.0` listé | | |
| 7 | Dépôt | `plakar at "$REPO" create` | repo créé | | |
| 8 | A: défs seules | backup `@kvmDefsOnly` | `domain.xml` présents | | |
| 9 | B: crash | backup `@kvmCrash` + `check` | disques + check OK | | |
| 10 | C: snapshot | backup `@kvmSnap` | check OK + overlay commité/supprimé | | |
| 11 | D: restore | `restore` + scp + redéfinition | VM redémarre, marqueur OK | | |
| 12 | DRBD | backup disque `/dev/drbdN` | disque sauvegardé, repl. intacte | | |
| 13 | Robustesse | tableau §11 | comportements attendus | | |

---

## 14. Dépannage

- **`make test` : erreurs `redeclared` dans `/usr/local/go/src/...`** → installation
  Go corrompue (tarball extrait par-dessus une ancienne). Réinstalle propre :
  ```bash
  rm -rf /usr/local/go
  cd /tmp && wget https://go.dev/dl/go1.24.5.linux-amd64.tar.gz
  tar -C /usr/local -xzf go1.24.5.linux-amd64.tar.gz
  export PATH=$PATH:/usr/local/go/bin && go version
  ```
- **SSH demande un mot de passe** → la clé n'est pas en place : refais `ssh-copy-id`,
  vérifie les permissions `~/.ssh` (700) et `authorized_keys` (600) sur l'HV.
- **`Guest agent is not responding`** → agent absent/arrêté dans la VM, ou canal
  virtio manquant (voir §1.4). Le mode `fsfreeze` bascule alors en crash (log).
- **`virsh: command not found` (côté plugin)** → installe `libvirt-clients` sur
  plakar-server.
- **`qemu-img: command not found`** côté conversion → il doit être présent **sur
  l'hyperviseur** (mode distant), pas sur plakar-server.
- **`qemu-img convert` lent / gros** → normal pour un premier full ; la dédup
  Kloset amortit l'espace des backups suivants. L'incrémental (dirty bitmaps) est
  au roadmap.
- **Restauration d'un disque DRBD** → l'image restaurée est un `qcow2` ;
  reconvertis vers le block device cible avec `qemu-img convert -O raw`.
- **Sous-commandes `source rm` / `pkg rm`** → confirmer l'orthographe avec
  `plakar help source` / `plakar help pkg` (peut varier selon la version).

---

## Annexe — Variante mode local

Si un jour tu installes Plakar **directement sur l'hyperviseur**, tout ce qui
précède reste valable en remplaçant `kvm://$HV/system` par `kvm:///system`, en
exécutant les commandes `[PLK]` sur l'hyperviseur, et en supprimant l'étape SSH
(§1.2). Aucune option supplémentaire : le mode est détecté automatiquement depuis
la location.

---

### Remonter les retours

Pour chaque KO : la commande, le message d'erreur complet, `plakar version`, et la
sortie de `virsh -c "$URI" domblklist "$DOM" --details`. Ça permettra d'ajuster le
plugin (parsing, modes, chemins, SSH).
