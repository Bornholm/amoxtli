---
source: https://fr.wikipedia.org/wiki/Go_(langage)
---

# Go (langage)

Go est un langage de programmation compilé et concurrent inspiré de C et Pascal. Il a été développé par Google à partir d’un concept initial de Robert Griesemer, Rob Pike et Ken Thompson.

Go veut faciliter et accélérer la programmation à grande échelle : en raison de sa simplicité, il est donc concevable de l’utiliser aussi bien pour écrire des applications, des scripts ou de grands systèmes. Cette simplicité est nécessaire aussi pour assurer la maintenance et l’évolution des programmes sur plusieurs générations de développeurs.

Cette dernière s'exprime à la fois dans le langage lui-même, avec une syntaxe simple, qui minimise le nombre d'abstractions et de fonctionnalités ; mais également dans la pile des technologies utilisées. Go dispose d'une bibliothèque standard particulièrement complète — qui s'enrichit à chaque mise à jour — et permet de réaliser de nombreuses tâches, sans nécessiter l'ajout de bibliothèques externes.

S’il vise aussi la rapidité d’exécution, indispensable à la programmation système, il considère le multithreading comme le moyen le plus robuste d’assurer sur les processeurs actuels cette rapidité tout en rendant la maintenance facile par séparation de tâches simples exécutées indépendamment afin d’éviter de créer des « usines à gaz ». Cette conception permet également le fonctionnement sans réécriture sur des architectures multi-cœurs en exploitant immédiatement l’augmentation de puissance correspondante.

## « Hello, world »

Voici un exemple d'un programme Hello world typique écrit en Go :

```go
package main

import "fmt"

func main() {
	fmt.Println("Hello, world")
}
```
