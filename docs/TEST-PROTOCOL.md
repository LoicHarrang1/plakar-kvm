# Protocole de test — intégration Plakar KVM/DRBD

Objectif : valider le connecteur **importer `kvm`** sur un hyperviseur KVM/DRBD
de test, en sauvegardant une VM dédiée dans un dépôt Kloset, puis en vérifiant
le contenu et la restauration.

> Version testée du plugin : **v0.1.0** (importer uniquement, mode local).
> À adapter si tu changes le numéro de version.

---

## 0. Convention & topologie

Chaque commande est préfixée par l'endroit où l'exécuter :

| Tag | Où |
|-----|----|
| `[HV]` | sur l'**hyperviseur** KVM/DRBD (nœud DRBD **Primary**) |
| `[VM]` | à l'intérieur de la **VM invitée** à sauvegarder |
| `[DEV]` | sur ta machine de build (peut être l'HV si Go y est installé) |

Hypothèses (à ajuster) :

- Nom de la VM de test : `plakar-test` → variable `DOM=plakar-test`
- Connexion libvirt locale : `qemu:///system` → location Plakar `kvm:///system`
- Dépôt Kloset : `/var/backups/kvm` sur l'HV
- Les disques de la VM sont soit des fichiers `qcow2`, soit des devices DRBD
  (`/dev/drbdN`) — le plugin gère les deux.

> ⚠️ **Exécuter Plakar sur le nœud Primary.** En mode local, le plugin lit les
> images disque directement sur le système de fichiers / le block device. Sur un
> Secondary, `/dev/drbdN` n'est pas lisible.

---

## 1. Prérequis

### 1.1 Sur l'hyperviseur `[HV]`

```bash
# outils requis dans le PATH
virsh --version
qemu-img --version
which virsh qemu-img

# la VM de test existe et son état
export DOM=plakar-test
virsh dominfo "$DOM"
virsh domblklist "$DOM" --details    # note les disques (Target / Source)
```

### 1.2 Préparer l'invité pour la cohérence applicative (mode `fsfreeze`)

Le mode `fsfreeze` (défaut) quiesce les FS de l'invité via le **qemu-guest-agent**.

`[VM]` — installer l'agent dans la VM :

```bash
# Debian/Ubuntu
sudo apt-get update && sudo apt-get install -y qemu-guest-agent
sudo systemctl enable --now qemu-guest-agent
# RHEL/Alma/Rocky
# sudo dnf install -y qemu-guest-agent && sudo systemctl enable --now qemu-guest-agent
```

`[HV]` — vérifier que le canal agent est présent et que freeze/thaw fonctionne :

```bash
virsh qemu-agent-command "$DOM" '{"execute":"guest-ping"}'   # -> {"return":{}}
virsh domfsfreeze "$DOM" && virsh domfsthaw "$DOM"           # doit réussir
```

> Si le canal manque, ajouter à la config de la VM (`virsh edit "$DOM"`) :
> ```xml
> <channel type='unix'>
>   <target type='virtio' name='org.qemu.guest_agent.0'/>
> </channel>
> ```
> puis redémarrer la VM. Sans agent, teste uniquement le mode `crash`.

---

## 2. Build & packaging du plugin `[DEV]`

Nécessite Go ≥ 1.24 et un accès réseau (résolution des dépendances).

```bash
git clone <URL_DE_TON_FORK>/integration-kvm.git
cd integration-kvm

go mod tidy          # résout les dépendances indirectes
make test            # tests unitaires (parsing, URI, modes) -> PASS attendu
make build           # produit ./kvmImporter

# packaging du plugin (produit un .ptar linux/amd64)
plakar pkg create manifest.yaml v0.1.0
ls -1 kvm_v0.1.0_*.ptar
```

Copier le `.ptar` sur l'hyperviseur si le build a été fait ailleurs :

```bash
scp kvm_v0.1.0_linux_amd64.ptar user@hyperviseur:/tmp/
```

---

## 3. Installation du plugin `[HV]`

```bash
plakar pkg add /tmp/kvm_v0.1.0_linux_amd64.ptar
plakar pkg show                      # doit lister kvm@v0.1.0
```

---

## 4. Création du dépôt Kloset `[HV]`

```bash
sudo mkdir -p /var/backups/kvm
plakar at /var/backups/kvm create    # définir une passphrase (à conserver !)
```

> La passphrase n'est stockée nulle part et est irrécupérable. Note-la.
> Pour éviter la saisie interactive pendant les tests, tu peux exporter :
> `export PLAKAR_PASSPHRASE='...'` (vérifie le nom exact avec `plakar help`).

---

## 5. Test A — smoke test : définitions seules (`include_disks=false`)

But : valider la connexion libvirt et l'export des XML sans toucher aux disques.

```bash
plakar source add kvmDefsOnly kvm:///system include_disks=false
plakar at /var/backups/kvm backup @kvmDefsOnly
plakar at /var/backups/kvm ls        # note le SNAPID
```

Vérification :

```bash
SNAP=<SNAPID>
plakar at /var/backups/kvm ls "$SNAP"          # arborescence: <domaine>/domain.xml
plakar at /var/backups/kvm cat "$SNAP:$DOM/domain.xml" | head -20
```

✅ Attendu : un `domain.xml` par domaine défini, contenu = sortie de `virsh dumpxml`.

---

## 6. Test B — backup crash-consistent (disques inclus)

But : sauvegarder disques + définitions sans quiescing (rapide).

```bash
plakar source add kvmCrash kvm:///system consistency=crash domains=$DOM
plakar at /var/backups/kvm backup @kvmCrash
plakar at /var/backups/kvm ls
```

Vérification :

```bash
SNAP=<SNAPID>
plakar at /var/backups/kvm ls "$SNAP:$DOM/disks"   # une entrée par disque
plakar at /var/backups/kvm check "$SNAP"           # intégrité (déchiffrement + checksums)
```

✅ Attendu : `<domaine>/disks/<nom-image>` présent, `check` = OK.

---

## 7. Test C — backup application-consistent (`fsfreeze`)

But : valider le gel/dégel invité + copie `qemu-img convert`.

Optionnel — poser un marqueur dans l'invité juste avant, pour vérifier la
cohérence temporelle après restauration :

```bash
# [VM]
date -u +%FT%TZ | sudo tee /root/plakar-marker.txt && sync
```

```bash
# [HV]
plakar source add kvmFreeze kvm:///system consistency=fsfreeze domains=$DOM
plakar at /var/backups/kvm backup @kvmFreeze
plakar at /var/backups/kvm ls
```

Pendant le backup, observer les logs : un `domfsfreeze` puis `domfsthaw` doivent
encadrer la conversion des disques. Fenêtre de gel = durée du `qemu-img convert`.

Vérification :

```bash
SNAP=<SNAPID>
plakar at /var/backups/kvm ls "$SNAP:$DOM/disks"   # fichiers en .qcow2 (copies compactes)
plakar at /var/backups/kvm check "$SNAP"
```

✅ Attendu : disques sous `<domaine>/disks/<nom>.qcow2`, `check` OK, la VM
n'est pas restée gelée (vérifier réactivité de la VM après coup).

---

## 8. Test D — restauration & validation réelle

> ⚠️ L'**exporter n'est pas encore implémenté** : la restauration extrait les
> fichiers (XML + images) dans un répertoire, puis on redéfinit la VM à la main.

```bash
# [HV]
mkdir -p /tmp/restore
plakar at /var/backups/kvm restore -to /tmp/restore "$SNAP"
find /tmp/restore -maxdepth 3 -type f
```

Redéfinition d'une VM de contrôle (⚠️ **ne pas écraser la VM de prod de test** —
renomme le domaine et les chemins) :

```bash
cd /tmp/restore/$DOM

# 1) Récupérer le disque restauré vers un emplacement de test
#    - si image fichier :
cp disks/*.qcow2 /var/lib/libvirt/images/${DOM}-restored.qcow2
#    - si disque DRBD raw à l'origine, réécrire l'image dans un volume dédié :
#      qemu-img convert -O raw disks/<img>.qcow2 /dev/<volume-de-test>

# 2) Adapter le XML : nouveau nom + nouveau chemin de disque + retirer l'UUID/MAC
cp domain.xml ${DOM}-restored.xml
sed -i "s/<name>$DOM<\/name>/<name>${DOM}-restored<\/name>/" ${DOM}-restored.xml
#   éditer <source .../> pour pointer vers le disque restauré, supprimer <uuid> et <mac>

# 3) Définir et démarrer
virsh define ${DOM}-restored.xml
virsh start  ${DOM}-restored
```

Validation :

```bash
# [VM restaurée] vérifier le marqueur posé en 7
cat /root/plakar-marker.txt
```

✅ Attendu : la VM restaurée démarre, le marqueur est présent et cohérent.

---

## 9. Vérifications spécifiques DRBD

```bash
# [HV] confirmer le rôle Primary (lecture des devices possible)
drbdadm status                       # ou: cat /proc/drbd
virsh domblklist "$DOM" --details    # une Source de type block = /dev/drbdN ?
```

- Lancer un backup (Test B) sur une VM dont un disque est un `/dev/drbdN` et
  confirmer que l'entrée `disks/drbdN` apparaît dans le snapshot.
- Vérifier que la réplication DRBD n'est pas perturbée pendant/après le backup
  (`drbdadm status` inchangé, pas de resync inattendu).

✅ Attendu : disque DRBD sauvegardé, réplication intacte.

---

## 10. Tests de robustesse / cas d'erreur

| Cas | Commande / condition | Attendu |
|-----|----------------------|---------|
| VM éteinte | `virsh shutdown $DOM` puis backup | lecture directe (mode crash forcé), backup OK |
| Filtre domaine inconnu | `... domains=nexistepas` | aucun domaine sauvegardé, pas de crash |
| Sans guest-agent + fsfreeze | agent arrêté, `consistency=fsfreeze` | fallback crash + log d'avertissement, backup OK |
| Mode snapshot | `consistency=snapshot` | erreur explicite « not yet implemented » (attendu à ce stade) |
| Mauvaise location | `kvm:///bogus` | rejet à la config (schema/erreur claire) |
| Ping | `plakar` doit joindre l'hyperviseur | échec propre si `virsh` inaccessible |

---

## 11. Nettoyage

```bash
# domaine de contrôle restauré
virsh destroy ${DOM}-restored 2>/dev/null; virsh undefine ${DOM}-restored
rm -f /var/lib/libvirt/images/${DOM}-restored.qcow2 ${DOM}-restored.xml
rm -rf /tmp/restore

# sources et dépôt de test
plakar source rm kvmDefsOnly kvmCrash kvmFreeze    # vérifier le sous-commande exact
rm -rf /var/backups/kvm                            # ⚠️ supprime tous les snapshots de test

# plugin (optionnel)
plakar pkg rm kvm                                  # vérifier le sous-commande exact
```

---

## 12. Grille de résultats (à remplir)

| # | Test | Commande clé | Résultat attendu | OK/KO | Notes |
|---|------|--------------|------------------|-------|-------|
| 1 | Prérequis outils | `virsh/qemu-img --version` | présents | | |
| 2 | Guest agent | `virsh domfsfreeze/thaw` | succès | | |
| 3 | Build + tests | `make test && make build` | PASS + binaire | | |
| 4 | Packaging | `plakar pkg create` | `.ptar` généré | | |
| 5 | Install | `plakar pkg show` | `kvm@v0.1.0` listé | | |
| 6 | Repo | `plakar at ... create` | repo créé | | |
| 7 | A: défs seules | backup `@kvmDefsOnly` | `domain.xml` présents | | |
| 8 | B: crash | backup `@kvmCrash` + `check` | disques + check OK | | |
| 9 | C: fsfreeze | backup `@kvmFreeze` | `.qcow2` + freeze/thaw | | |
| 10 | D: restore | `restore -to` + redéfinition | VM redémarre, marqueur OK | | |
| 11 | DRBD | backup disque `/dev/drbdN` | disque sauvegardé, repl. intacte | | |
| 12 | Robustesse | tableau §10 | comportements attendus | | |

---

## 13. Dépannage

- **`virsh: command not found`** dans le plugin → installe libvirt-clients, ou
  vérifie le `PATH` du process qui lance Plakar.
- **fsfreeze échoue** → agent absent/arrêté ; le plugin bascule en crash et logge
  un avertissement (ce n'est pas bloquant).
- **`qemu-img convert` lent / gros** → normal pour un premier full ; les backups
  suivants profitent de la déduplication Kloset. L'incrémental (dirty bitmaps)
  est au roadmap.
- **Sous-commandes `source rm` / `pkg rm`** → confirmer l'orthographe exacte avec
  `plakar help source` et `plakar help pkg` (peut varier selon la version).
- **Restauration d'un disque DRBD** → l'image restaurée est un `qcow2` ; reconvertis
  vers le block device cible avec `qemu-img convert -O raw`.

---

### Remonter les retours

Note pour chaque KO : la commande, le message d'erreur complet, la version de
Plakar (`plakar version`), et la sortie de `virsh domblklist $DOM --details`.
Ces éléments permettront d'ajuster le plugin (parsing, modes, chemins).
